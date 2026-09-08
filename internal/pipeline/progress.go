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

	// maxStreak is how many failures in a row end the pass; zero disables the
	// breaker. streak counts them, and onTrip is called once when the limit is
	// reached. The download leg needs this for the same reason the upload leg
	// does: when the source stops serving files every item fails in
	// milliseconds, and without a breaker a pass burns the entire remaining
	// todo list on transfers that cannot succeed.
	maxStreak int
	streak    int
	tripped   bool
	onTrip    func()

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
		p.streak++
	} else {
		p.stats.Done++
		p.streak = 0
	}
	p.outcomes = append(p.outcomes, Outcome{Item: el.item, Err: err})
	stats := p.stats
	// Decided under the lock and acted on outside it: onTrip stops the
	// iterator, and holding this mutex across it would put the callback's
	// locking order inside this one's.
	trip := p.maxStreak > 0 && p.streak >= p.maxStreak && !p.tripped
	if trip {
		p.tripped = true
	}
	p.mu.Unlock()

	if trip && p.onTrip != nil {
		p.onTrip()
	}
	p.events.DownloadDone(el.item, err)
	p.events.Stats(stats)
}

// refused records an item that failed before any transfer could start.
//
// It exists so that "the message could not be re-read" counts as the same kind
// of event as "the transfer died", because operationally it is: both mean
// Telegram is not serving this file right now, and both are usually the same
// outage. An item that never reaches a worker never reaches OnAdd or OnDone, so
// without this it would move no counter, feed no streak, produce no Outcome for
// Run to retry, and leave the live report on a still frame while the pass walked
// the rest of the list.
//
// Everything OnDone does for a failure it does here, minus the file: the item is
// counted as started and failed, its bytes join the total the report is working
// towards, the streak advances so a dead source still trips the breaker, and the
// Outcome is what makes Run try the item again on the next pass.
func (p *progress) refused(it tgsource.Item, err error) {
	p.mu.Lock()
	p.stats.Started++
	p.stats.BytesTotal += it.Size()
	p.stats.Failed++
	p.streak++
	p.outcomes = append(p.outcomes, Outcome{Item: it, Err: err})
	stats := p.stats
	// Decided under the lock and acted on outside it, as in OnDone.
	trip := p.maxStreak > 0 && p.streak >= p.maxStreak && !p.tripped
	if trip {
		p.tripped = true
	}
	p.mu.Unlock()

	if trip && p.onTrip != nil {
		p.onTrip()
	}
	// Reported as a start and an immediate end rather than as an end alone: a
	// reporter tracking which files are in flight is entitled to see every item
	// it is told about finish, and one it never saw start would be a stray.
	p.events.DownloadStart(it)
	p.events.DownloadDone(it, err)
	p.events.Stats(stats)
}

// brokeCircuit reports whether the failure streak ended this pass.
func (p *progress) brokeCircuit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tripped
}

func (p *progress) results() ([]Outcome, Stats) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outcomes, p.stats
}
