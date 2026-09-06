// Package report renders a run's progress for whoever is watching.
package report

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tiennm99dev/telegram-exporter/internal/pipeline"
)

// statsInterval is how often a redirected run prints a line.
//
// The shell pipeline learned this the hard way: tdl's progress bar is ANSI
// redraws, which are right on a terminal and turn a captured log into
// megabytes of control characters. So a TTY gets a redrawn line and everything
// else gets a periodic summary.
const statsInterval = 30 * time.Second

// Reporter renders progress, adapting to whether it is writing to a terminal.
type Reporter struct {
	w          io.Writer
	tty        bool
	total      int
	totalBytes int64

	mu       sync.Mutex
	started  time.Time
	lastLine time.Time
}

// New builds a reporter for w, which is treated as a terminal when it is one.
//
// The totals are passed in rather than taken from Stats because Stats.BytesTotal
// only counts items the downloader has started, so a progress line built from it
// shows a denominator that grows as the run proceeds — "0 B of 52 KiB" on a run
// that will move gigabytes. The caller knows the real figures before starting.
func New(w io.Writer, total int, totalBytes int64) *Reporter {
	return &Reporter{w: w, tty: isTerminal(w), total: total, totalBytes: totalBytes, started: time.Now()}
}

// Update renders a snapshot. Safe to call from several goroutines, and cheap
// enough to call on every progress callback.
func (r *Reporter) Update(s pipeline.Stats) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if r.tty {
		// \r rather than \n: one line, redrawn.
		fmt.Fprintf(r.w, "\r\033[K%s", r.line(s, now))
		return
	}
	if now.Sub(r.lastLine) < statsInterval {
		return
	}
	r.lastLine = now
	fmt.Fprintf(r.w, "%s\n", r.line(s, now))
}

// Finish writes the closing summary, ending the redrawn line if there was one.
func (r *Reporter) Finish(s pipeline.Stats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tty {
		fmt.Fprint(r.w, "\r\033[K")
	}
	elapsed := time.Since(r.started).Round(time.Second)
	fmt.Fprintf(r.w, "%d done, %d failed, %s in %s (%s/s)\n",
		s.Done, s.Failed, humanBytes(s.BytesDone), elapsed,
		humanBytes(int64(float64(s.BytesDone)/max(elapsed.Seconds(), 1))))
}

func (r *Reporter) line(s pipeline.Stats, now time.Time) string {
	elapsed := now.Sub(r.started)
	rate := float64(s.BytesDone) / max(elapsed.Seconds(), 1)
	return fmt.Sprintf("%d/%d done, %d failed, %s of %s, %s/s",
		s.Done, r.total, s.Failed,
		humanBytes(s.BytesDone), humanBytes(r.totalBytes), humanBytes(int64(rate)))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// isTerminal reports whether w is a character device.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
