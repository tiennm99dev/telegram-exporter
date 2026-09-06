package pipeline

import (
	"sync"

	"github.com/iyear/tdl/core/downloader"

	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// Outcome is what happened to one item.
type Outcome struct {
	Item tgsource.Item
	Err  error // nil when the file downloaded and was renamed into place
}

// Stats is a snapshot of a run's progress.
type Stats struct {
	Started    int
	Done       int
	Failed     int
	BytesDone  int64
	BytesTotal int64
}

// progress collects per-item results and feeds an optional live reporter.
//
// The downloader calls these from its worker goroutines, so everything here is
// mutex-guarded. OnDone fires once per item whether it succeeded or not, which
// is what makes it the right place to finish the file — the downloader itself
// never closes or renames what To() handed it.
type progress struct {
	mu       sync.Mutex
	stats    Stats
	outcomes []Outcome
	inFlight map[int]int64 // message id -> bytes written so far

	finish func(*elem, error) error
	events Events
}

func newProgress(finish func(*elem, error) error, events Events) *progress {
	if events == nil {
		events = nopEvents{}
	}
	return &progress{
		inFlight: make(map[int]int64),
		finish:   finish,
		events:   events,
	}
}

func (p *progress) OnAdd(e downloader.Elem) {
	el := e.(*elem)
	p.mu.Lock()
	p.stats.Started++
	p.stats.BytesTotal += el.item.Size()
	stats := p.stats
	p.mu.Unlock()
	p.events.DownloadStart(el.item)
	p.events.Stats(stats)
}

func (p *progress) OnDownload(e downloader.Elem, state downloader.ProgressState) {
	el := e.(*elem)
	p.mu.Lock()
	// State carries the running total for this item, not a delta, so the
	// aggregate is adjusted by the difference since the last callback.
	prev := p.inFlight[el.item.MessageID]
	p.inFlight[el.item.MessageID] = state.Downloaded
	p.stats.BytesDone += state.Downloaded - prev
	stats := p.stats
	p.mu.Unlock()
	p.events.DownloadBytes(el.item, state.Downloaded)
	p.events.Stats(stats)
}

func (p *progress) OnDone(e downloader.Elem, err error) {
	el := e.(*elem)

	// Closing and renaming happens here because this is the only callback that
	// runs exactly once per item and knows whether it succeeded.
	if ferr := p.finish(el, err); ferr != nil && err == nil {
		err = ferr
	}

	p.mu.Lock()
	delete(p.inFlight, el.item.MessageID)
	if err != nil {
		p.stats.Failed++
	} else {
		p.stats.Done++
	}
	p.outcomes = append(p.outcomes, Outcome{Item: el.item, Err: err})
	stats := p.stats
	p.mu.Unlock()
	p.events.DownloadDone(el.item, err)
	p.events.Stats(stats)
}

func (p *progress) results() ([]Outcome, Stats) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outcomes, p.stats
}
