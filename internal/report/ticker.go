package report

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// tickerInterval is how often a terminal redraws a phase counter. Fast enough
// to look alive, slow enough not to matter.
const tickerInterval = 250 * time.Millisecond

// Ticker reports progress through a long phase that would otherwise be silent.
//
// Reading a chat's history and listing a remote each take minutes on an archive
// of any size, and both used to print nothing between their opening line and
// their result. A run that is working looked identical to one that had hung, so
// the only way to tell was to wait it out.
//
// Cadence follows the same rule as Reporter: a terminal gets a redrawn line, a
// redirected run gets a periodic one, because ANSI redraws turn a captured log
// into megabytes of control characters.
type Ticker struct {
	w     io.Writer
	tty   bool
	noun  string
	start time.Time

	mu       sync.Mutex
	lastLine time.Time
}

// NewTicker builds a ticker that counts noun, e.g. "messages" or "objects".
func NewTicker(w io.Writer, noun string) *Ticker {
	return &Ticker{w: w, tty: isTerminal(w), noun: noun, start: time.Now()}
}

// Update reports a running count. Safe to call from several goroutines, and
// cheap enough to call per item.
func (t *Ticker) Update(n int) {
	if !t.mu.TryLock() {
		return
	}
	defer t.mu.Unlock()

	now := time.Now()
	interval := statsInterval
	if t.tty {
		interval = tickerInterval
	}
	if now.Sub(t.lastLine) < interval {
		return
	}
	t.lastLine = now

	if t.tty {
		fmt.Fprintf(t.w, "\r\033[K  %s %s...", humanCount(n), t.noun)
		return
	}
	fmt.Fprintf(t.w, "  %s %s...\n", humanCount(n), t.noun)
}

// Done clears the redrawn line and states the final count.
func (t *Ticker) Done(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tty {
		fmt.Fprint(t.w, "\r\033[K")
	}
	fmt.Fprintf(t.w, "  %s %s in %s\n", humanCount(n), t.noun,
		time.Since(t.start).Round(time.Second))
}

// humanCount groups thousands, so 12000 reads as 12,000.
func humanCount(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	out := make([]byte, 0, len(s)+len(s)/3)
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
