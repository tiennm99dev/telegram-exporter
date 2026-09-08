// Package report renders a run's progress for whoever is watching.
package report

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tiennm99dev/telegram-exporter/internal/pipeline"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// statsInterval is how often a redirected run prints a line.
//
// The shell pipeline learned this the hard way: tdl's progress bar is ANSI
// redraws, which are right on a terminal and turn a captured log into
// megabytes of control characters. So a TTY gets a redrawn line and everything
// else gets a periodic summary.
const statsInterval = 30 * time.Second

// Reporter renders progress as periodic plain-text lines.
//
// This is the redirected-output path. Bars are deliberately absent: they are
// continuous ANSI cursor movement, and a captured log of them is megabytes of
// control characters — which is exactly what tdl's progress bar did to the shell
// pipeline's logs.
type Reporter struct {
	w          io.Writer
	total      int
	totalBytes int64

	legs legTotals

	mu       sync.Mutex
	started  time.Time
	lastLine time.Time
}

// Events builds the renderer suited to w: bars on a terminal, periodic lines
// anywhere else.
//
// The totals are passed in rather than taken from Stats because Stats.BytesTotal
// only counts items the downloader has started, so a progress line built from it
// shows a denominator that grows as the run proceeds — "0 B of 52 KiB" on a run
// that will move gigabytes. The caller knows the real figures before starting.
func Events(w io.Writer, total int, totalBytes int64) interface {
	pipeline.Events
	Finish(pipeline.Stats)
} {
	if isTerminal(w) {
		return NewLive(w, total, totalBytes)
	}
	return newReporter(w, total, totalBytes)
}

func newReporter(w io.Writer, total int, totalBytes int64) *Reporter {
	return &Reporter{w: w, total: total, totalBytes: totalBytes, started: time.Now()}
}

// Stats renders a snapshot. Safe to call from several goroutines.
//
// A contended update is dropped rather than queued. Every download worker calls
// this on each progress callback, so holding the lock across the write would
// make write latency throttle the downloads themselves. A skipped frame costs
// nothing; the next callback is milliseconds away and Finish always prints.
func (r *Reporter) Stats(s pipeline.Stats) {
	r.legs.setDownload(s.Done+s.Failed, s.BytesDone)

	if !r.mu.TryLock() {
		return
	}
	defer r.mu.Unlock()

	now := time.Now()
	if now.Sub(r.lastLine) < statsInterval {
		return
	}
	r.lastLine = now
	for _, line := range r.lines(now) {
		fmt.Fprintf(r.w, "%s\n", line)
	}
}

// The per-file events are recorded as one line each rather than a bar. At one
// line per file this stays readable in a log, and it is what makes a captured
// run auditable afterwards: which files moved, in what order, and which failed.
func (r *Reporter) DownloadStart(tgsource.Item)        {}
func (r *Reporter) DownloadBytes(tgsource.Item, int64) {}
func (r *Reporter) UploadStart(tgsource.Item)          {}

func (r *Reporter) DownloadDone(it tgsource.Item, err error) {
	if err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		fmt.Fprintf(r.w, "  download failed  %q: %v\n", it.Name, err)
	}
}

func (r *Reporter) UploadDone(it tgsource.Item, err error) {
	if err == nil {
		r.legs.addUpload(it.Size())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		fmt.Fprintf(r.w, "  upload failed    %q: %v\n", it.Name, err)
		return
	}
	fmt.Fprintf(r.w, "  archived  %-10s %q\n", humanBytes(it.Size()), it.Name)
}

func (r *Reporter) Retry(attempt, files int, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.w, "  retrying %s file(s) in %s — attempt %d\n",
		humanCount(files), wait.Round(time.Second), attempt)
}

// Finish writes the closing summary.
func (r *Reporter) Finish(s pipeline.Stats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	writeSummary(r.w, s, time.Since(r.started))
}

// lines renders one line per leg. Separately, because the two run at different
// speeds and a single combined figure hides which of them is the bottleneck —
// the question actually being asked when a run slows down.
func (r *Reporter) lines(now time.Time) []string {
	elapsed := now.Sub(r.started)
	dlFiles, dlBytes, upFiles, upBytes := r.legs.snapshot()
	return []string{
		r.leg("download", dlFiles, dlBytes, elapsed),
		r.leg("upload  ", upFiles, upBytes, elapsed),
	}
}

func (r *Reporter) leg(label string, files int, bytes int64, elapsed time.Duration) string {
	rate := float64(bytes) / max(elapsed.Seconds(), 1)
	eta := "—"
	if rate > 0 && r.totalBytes > bytes {
		eta = time.Duration(float64(r.totalBytes-bytes) / rate * float64(time.Second)).
			Round(time.Second).String()
	}
	return fmt.Sprintf("  %s %s/%s files, %s of %s, %s/s, ETA %s",
		label, humanCount(files), humanCount(r.total),
		humanBytes(bytes), humanBytes(r.totalBytes), humanBytes(int64(rate)), eta)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	// The unit table runs to exabytes so the index cannot escape it. A PiB is
	// not reachable from a Telegram chat, but a panic in the progress line would
	// take down a run that was working.
	const units = "KMGTPE"
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < len(units)-1; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), units[exp])
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

// writeSummary prints the closing line both renderers end with.
func writeSummary(w io.Writer, s pipeline.Stats, elapsed time.Duration) {
	elapsed = elapsed.Round(time.Second)
	fmt.Fprintf(w, "%s done, %s failed, %s in %s (%s/s)\n",
		humanCount(s.Done), humanCount(s.Failed), humanBytes(s.BytesDone), elapsed,
		humanBytes(int64(float64(s.BytesDone)/max(elapsed.Seconds(), 1))))
}

// HumanBytes and HumanCount are the shared formatters, exported so the commands
// print the same shapes as the progress renderers do.
func HumanBytes(n int64) string { return humanBytes(n) }
func HumanCount(n int) string   { return humanCount(n) }
