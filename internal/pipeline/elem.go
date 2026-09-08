// Package pipeline drives the download half of an archive run.
package pipeline

import (
	"context"
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
	var item tgsource.Item
	for {
		if i.failure != nil || i.stopped.Load() {
			return false
		}
		if err := ctx.Err(); err != nil {
			i.failure = err
			return false
		}

		var (
			err error
			ok  bool
		)
		item, err, ok = i.next()
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
			if len(i.skipped) < maxRecordedErrors {
				i.skipped = append(i.skipped, fmt.Errorf("message %d: %w", item.MessageID, err))
			}
			continue
		}
		break
	}

	if i.acquire != nil {
		if err := i.acquire(ctx, item.Size()); err != nil {
			i.failure = err
			return false
		}
	}

	f, err := os.OpenFile(partPath(i.staging, item), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		// The reservation is handed back here because this item will never
		// reach a download, so no OnDone will ever release it for us.
		if i.release != nil {
			i.release(item.Size())
		}
		i.failure = fmt.Errorf("open destination for message %d: %w", item.MessageID, err)
		return false
	}
	i.opened = append(i.opened, f)

	i.current = &elem{item: item, file: f, takeout: i.takeout}
	return true
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
