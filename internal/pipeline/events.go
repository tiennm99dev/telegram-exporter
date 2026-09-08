package pipeline

import (
	"time"

	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// Events receives a run's per-item lifecycle.
//
// Aggregate Stats alone cannot say what a run is doing right now — which files
// are moving, how far along each is, whether the time is going into downloads or
// uploads. On an archive that runs for hours those are the questions being
// asked, and a single "26/2613 done" line answers none of them.
//
// Every method is called from a worker goroutine, several at once, and some of
// them fire per network chunk. Implementations must be safe to call
// concurrently and must not block: a slow renderer would throttle the transfers
// it is describing.
type Events interface {
	// Stats reports the running totals.
	Stats(Stats)

	DownloadStart(it tgsource.Item)
	// DownloadBytes reports the total written for it so far, not a delta.
	DownloadBytes(it tgsource.Item, done int64)
	DownloadDone(it tgsource.Item, err error)

	UploadStart(it tgsource.Item)
	UploadDone(it tgsource.Item, err error)

	// Retry announces another pass over the items the last one failed, and how
	// long the run waits first. It is the one event that is not per-item, and it
	// exists because the wait is measured in minutes: a display that went quiet
	// for that long with no explanation is indistinguishable from a hang.
	Retry(attempt, files int, wait time.Duration)
}

// nopEvents is used when a caller wants no reporting, so nothing on the hot
// path has to nil-check.
type nopEvents struct{}

func (nopEvents) Stats(Stats)                        {}
func (nopEvents) DownloadStart(tgsource.Item)        {}
func (nopEvents) DownloadBytes(tgsource.Item, int64) {}
func (nopEvents) DownloadDone(tgsource.Item, error)  {}
func (nopEvents) UploadStart(tgsource.Item)          {}
func (nopEvents) UploadDone(tgsource.Item, error)    {}
func (nopEvents) Retry(int, int, time.Duration)      {}
