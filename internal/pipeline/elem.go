// Package pipeline drives the download half of an archive run.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/gotd/td/tg"

	"github.com/iyear/tdl/core/downloader"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// partSuffix marks a download that is still in flight.
//
// Deliberately not tdl's ".tmp": nothing here excludes by extension any more,
// because upload is triggered by a download returning rather than by a filter
// over a directory. A distinct suffix just keeps a staging directory shared with
// a legacy tdl run unambiguous during the cutover.
//
// It is defined in naming because naming.Safe's length limit has to leave room
// for it — a name that fits but whose part file does not would pass the check
// and then fail to open, stalling the walk on that message forever.
const partSuffix = naming.PartSuffix

// elem adapts one media item to the downloader's element interface.
type elem struct {
	item    tgsource.Item
	file    *os.File
	takeout bool

	// cause is why the transfer failed, when the downloader logged a reason.
	// It is the only route that reason has to reach finish, because core's
	// Download logs it and returns nil; see captureCauses. Written by the log
	// call on the download worker's goroutine and read by that worker's OnDone,
	// so it needs no lock.
	cause error
}

func (e *elem) File() downloader.File { return mediaFile{e.item} }
func (e *elem) To() io.WriterAt       { return e.file }
func (e *elem) AsTakeout() bool       { return e.takeout }

// mediaFile exposes what the downloader needs to locate the bytes. All three
// values come straight from tmedia, so nothing is looked up a second time.
type mediaFile struct{ item tgsource.Item }

func (f mediaFile) Location() tg.InputFileLocationClass { return f.item.Media.InputFileLoc }
func (f mediaFile) Size() int64                         { return f.item.Media.Size }
func (f mediaFile) DC() int                             { return f.item.Media.DC }

// partPath and finalPath are where an item is written and where it lands.
func partPath(staging string, it tgsource.Item) string {
	return filepath.Join(staging, it.Name+partSuffix)
}
func finalPath(staging string, it tgsource.Item) string {
	return filepath.Join(staging, it.Name)
}

// elemIter turns the item sequence into the pull iterator the downloader wants,
// opening each destination file as it goes.
//
// The downloader consumes Next/Value/Err; Walk produces an iter.Seq2. iter.Pull2
// bridges them without this code owning a goroutine or a channel, which is why
// Walk returns a sequence in the first place.
type elemIter struct {
	next     func() (tgsource.Item, error, bool)
	stopPull func()
	staging  string
	takeout  bool

	// refresh re-mints an item's file reference just before its download; see
	// reminted. Unset leaves the walk's own reference in place.
	refresh tgsource.Refresh
	// refused records an item that failed before any transfer could start, so it
	// counts as a failure rather than disappearing. Unset means such an item is
	// only skipped.
	refused func(tgsource.Item, error)

	// acquire reserves staging space for the next item. Blocking here is what
	// makes backpressure work: core's Download calls Next from its dispatch
	// loop (downloader.go:38), so a blocked Next stops new downloads starting
	// without stopping the uploads that free the space.
	acquire func(context.Context, int64) error
	// release hands a reservation back when the item never reaches a download.
	release func(int64)

	current *elem

	// failure holds why iteration stopped, and Err deliberately does not return
	// it. core's Download skips wg.Wait entirely when Iter.Err is non-nil
	// (downloader.go:65-68), abandoning workers that are still running — which
	// would let this package tear down its upload channel underneath them. So
	// Next reports "no more items" and the caller reads failure() afterwards,
	// guaranteeing every worker has finished first.
	failure error
	// stopped ends iteration without an error, for a caller that has decided the
	// run cannot usefully continue. Supplied by the caller so it can be set from
	// another goroutine without racing on the iterator itself.
	stopped *atomic.Bool

	// skipped collects items refused before any download was attempted. They do
	// not stop the run: one message with a hostile filename must not be able to
	// strand every message behind it, which is what ending iteration would mean.
	skipped []error

	// opened records every file handle. finish closes each one on the normal
	// path, so this is not what keeps descriptors from leaking; it is the
	// backstop for items that were opened but never reached finish, which is
	// what an aborted iteration leaves behind.
	opened []*os.File
}

func newElemIter(seq iter.Seq2[tgsource.Item, error], staging string, takeout bool) *elemIter {
	next, stopPull := iter.Pull2(seq)
	return &elemIter{
		next:     next,
		stopPull: stopPull,
		staging:  staging,
		takeout:  takeout,
		stopped:  new(atomic.Bool),
	}
}

func (i *elemIter) Next(ctx context.Context) bool {
	for {
		if i.failure != nil || i.stopped.Load() {
			return false
		}
		if err := ctx.Err(); err != nil {
			i.failure = err
			return false
		}

		item, err, ok := i.next()
		if !ok {
			return false
		}
		if err != nil {
			i.failure = err
			return false
		}

		// A name that cannot be written is refused here rather than left to
		// os.Create, and refusing it skips the item rather than ending the walk.
		// selectTodo already filters these out on the CLI path, so reaching this
		// is either a second caller or a gap there; in both cases one unwritable
		// name must not strand the rest of the chat behind it.
		if err := naming.Safe(item.Name); err != nil {
			i.skip(item, err)
			continue
		}

		if i.acquire != nil {
			if err := i.acquire(ctx, item.Size()); err != nil {
				i.failure = err
				return false
			}
		}

		// The refreshed item replaces the original only on success. A failed
		// refresh reports a zero Item, and handing that to the reservation or to
		// the failure record would release nothing and blame message 0.
		fresh, err := i.reminted(ctx, item)
		if err == nil {
			item = fresh
		} else {
			// Nothing was staged and nothing will be, so the reservation goes
			// back before anything else; otherwise the cap ends the run
			// over-committed by every item that failed here.
			i.giveBack(item)
			if ctx.Err() != nil {
				i.failure = err
				return false
			}
			// A message that is gone is skipped, for the same reason an
			// unwritable name is: it is a permanent, per-message fact, and one
			// of them must not strand every message behind it. Nothing is
			// archived for it, so the run's closing verify still reports it as
			// outstanding.
			//
			// Anything else is a failure and is recorded as one. Routing it
			// there rather than onto the skip path is what keeps the breaker,
			// the run's retry passes and the progress report working: a re-read
			// fails because the connection did, which is exactly when every
			// download is failing too, and a skip is invisible to all three — a
			// source outage would otherwise walk the whole todo list one dead
			// round trip at a time and still exit as merely incomplete.
			if errors.Is(err, tgsource.ErrGone) {
				i.skip(item, err)
			} else if i.refused != nil {
				i.refused(item, err)
			}
			continue
		}

		f, err := os.OpenFile(partPath(i.staging, item), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			// The reservation is handed back here because this item will never
			// reach a download, so no OnDone will ever release it for us.
			i.giveBack(item)
			i.failure = fmt.Errorf("open destination for message %d: %w", item.MessageID, err)
			return false
		}
		i.opened = append(i.opened, f)

		i.current = &elem{item: item, file: f, takeout: i.takeout}
		return true
	}
}

// reminted replaces an item's file reference with one Telegram has just issued.
//
// The reference in Media.InputFileLoc was minted when the walk saw the message,
// and Telegram expires those. The lifetime is undocumented and was observed to
// outlast an hour but not two, which is less than a run over a large chat spends
// downloading — so by the time the dispatch loop reaches an item near the end of
// the list its reference is dead, every remaining fetch returns
// FILE_REFERENCE_EXPIRED, and the breaker reads that as a dead source and
// abandons everything still to come.
//
// It is called as late as Next can leave it, and in particular after the budget
// acquire rather than before. core's downloader reads Elem.File().Location()
// once per attempt, inside the worker (downloader.go:89), so the only wait left
// between here and there is for a free worker slot, which one download bounds.
// Re-minting before the acquire would reintroduce the very failure this exists
// to prevent: that acquire is the run's brake and blocks for as long as the
// destination is slow, which on a stalled remote is long enough to expire a
// fresh token all over again.
//
// One round trip per transfer, and no guess at how long a reference lives.
// Retries get it for free: Run feeds a failed item straight back through
// Download, so a reference that died mid-transfer — a multi-gigabyte file can
// outlive its own token — is re-minted on the next pass rather than replayed.
func (i *elemIter) reminted(ctx context.Context, item tgsource.Item) (tgsource.Item, error) {
	if i.refresh == nil {
		return item, nil
	}
	return i.refresh(ctx, item)
}

// skip records an item no run will ever fetch, without ending this one.
func (i *elemIter) skip(item tgsource.Item, err error) {
	if len(i.skipped) < maxRecordedErrors {
		i.skipped = append(i.skipped, fmt.Errorf("message %d: %w", item.MessageID, err))
	}
}

// giveBack returns an item's staging reservation.
func (i *elemIter) giveBack(item tgsource.Item) {
	if i.release != nil {
		i.release(item.Size())
	}
}

func (i *elemIter) Value() downloader.Elem { return i.current }

// Err always reports nil so core's Download reaches wg.Wait and joins its
// workers. See the failure field.
func (i *elemIter) Err() error { return nil }

// Close releases the pull iterator and every file the walk opened.
func (i *elemIter) Close() error {
	i.stopPull()
	var firstErr error
	for _, f := range i.opened {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
