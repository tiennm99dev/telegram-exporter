// Package pipeline drives the download half of an archive run.
package pipeline

import (
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"

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
const partSuffix = ".part"

// elem adapts one media item to the downloader's element interface.
type elem struct {
	item    tgsource.Item
	file    *os.File
	takeout bool
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
	next    func() (tgsource.Item, error, bool)
	stop    func()
	staging string
	takeout bool

	current *elem
	err     error

	// opened records every file handle so a run can close them all. The
	// downloader never closes what To() hands it, and a leak here is thousands
	// of descriptors on a full archive run.
	opened []*os.File
}

func newElemIter(seq iter.Seq2[tgsource.Item, error], staging string, takeout bool) *elemIter {
	next, stop := iter.Pull2(seq)
	return &elemIter{next: next, stop: stop, staging: staging, takeout: takeout}
}

func (i *elemIter) Next(ctx context.Context) bool {
	if i.err != nil {
		return false
	}
	if err := ctx.Err(); err != nil {
		i.err = err
		return false
	}

	item, err, ok := i.next()
	if !ok {
		return false
	}
	if err != nil {
		i.err = err
		return false
	}

	// A name that cannot be written is refused here rather than left to
	// os.Create: the error names the message, and the run continues instead of
	// failing on a path that could never have worked.
	if err := naming.Safe(item.Name); err != nil {
		i.err = fmt.Errorf("message %d: %w", item.MessageID, err)
		return false
	}

	f, err := os.OpenFile(partPath(i.staging, item), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		i.err = fmt.Errorf("open destination for message %d: %w", item.MessageID, err)
		return false
	}
	i.opened = append(i.opened, f)

	i.current = &elem{item: item, file: f, takeout: i.takeout}
	return true
}

func (i *elemIter) Value() downloader.Elem { return i.current }
func (i *elemIter) Err() error             { return i.err }

// Close releases the pull iterator and every file the walk opened.
func (i *elemIter) Close() error {
	i.stop()
	var firstErr error
	for _, f := range i.opened {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
