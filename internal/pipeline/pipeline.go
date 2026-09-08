package pipeline

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/fs"
	"golang.org/x/sync/semaphore"

	"github.com/iyear/tdl/core/dcpool"

	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// Options configures a full download-and-upload run.
type Options struct {
	Pool    dcpool.Pool
	Dst     fs.Fs
	Staging string

	Threads int   // connections per file
	Limit   int   // files downloading at once
	Uploads int   // files uploading at once
	Budget  int64 // bytes allowed in staging at once; 0 means unbounded

	Confirm bool // re-state each uploaded object to prove its size
	Takeout bool

	// MaxFailures trips the run after this many consecutive upload failures.
	// Zero uses the shell pipeline's default of 5.
	MaxFailures int

	// FreeBytes and MinFree stop the run when the destination fills mid-way.
	// Both must be set for the check to happen; a backend that cannot report a
	// quota counts as unlimited.
	FreeBytes func(context.Context) (int64, bool)
	MinFree   int64

	// Events, when set, receives the run's per-item lifecycle: which files are
	// downloading, which are uploading, and how far along each one is.
	Events Events

	// Refresh, when set, re-reads a message for a live file reference just
	// before its download starts. A run over a large chat outlasts the
	// references its walk collected, so without this every fetch past that point
	// fails with FILE_REFERENCE_EXPIRED; see elemIter.Next.
	Refresh tgsource.Refresh
}

// Result is what a run achieved.
type Result struct {
	Stats    Stats
	Outcomes []Outcome
}

// Failed lists the items that did not make it to the remote.
func (r Result) Failed() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Err != nil {
			out = append(out, o)
		}
	}
	return out
}

// ErrDestinationFailing marks a run stopped because the destination refused
// upload after upload. It is distinct from an ordinary upload failure because
// the right response differs: a transient error is worth retrying, while a
// remote that is full, unreachable, or refusing credentials will refuse the next
// pass identically, and a driver that retries walks the whole chat and downloads
// gigabytes for nothing every time.
var ErrDestinationFailing = errors.New("destination stopped accepting uploads")

// ErrSourceFailing marks a run stopped because Telegram failed download after
// download. It is the download leg's counterpart to ErrDestinationFailing and
// means the same thing to a driver — another pass will fail the same way, so
// retrying on "incomplete" only re-walks the chat for nothing — but it points at
// the other half of the run, which is what an operator needs to know first.
//
// It survives the run's own retries: a pass that trips and then recovers reports
// nothing, so this only appears when the last attempt was still failing.
var ErrSourceFailing = errors.New("telegram stopped serving downloads")

// downloadAttempts is how many passes a run makes over the items it failed to
// fetch.
//
// Retrying inside the run is worth far more than leaving it to the next one: a
// fresh sync re-walks every message and re-indexes the whole remote before it
// can fetch a byte, while a retry here already has both. The failure this exists
// for is the transient one — a dead connection or a source that stops serving
// files for a few minutes takes down every transfer in flight, and every one of
// them is fetchable again afterwards.
const downloadAttempts = 3

// retryDelay spaces the passes out, and is a var so tests need not sleep.
//
// Minutes rather than seconds: the client's own recovery gives up on a broken
// connection only after the reconnect timeout (five minutes by default), so a
// pass that starts seconds after the last one failed is a pass into the same
// dead connection.
var retryDelay = func(attempt int) time.Duration {
	return time.Duration(attempt) * time.Minute
}

// maxRecordedErrors bounds what a run keeps from a failing remote. Past this,
// the pattern is established and joining thousands of identical strings just
// makes the final message unreadable.
const maxRecordedErrors = 10

// download is the download step, indirected so a test can drive Run's
// composition without a live Telegram connection. What that buys is coverage of
// the three properties Run alone is responsible for — that the upload channel is
// closed only after every send, that the byte budget balances across a whole
// run, and that a tripped breaker still terminates — none of which the pieces
// can be tested for individually.
var download = Download

// Run downloads every item and uploads each one as it completes.
//
// This is the whole reason for the rewrite. run.sh could not see inside tdl, so
// it inferred completion from a filename suffix plus a file's age, polled
// `du -sk` every ten seconds, and enforced its disk cap by sending SIGSTOP and
// SIGCONT to the tdl process. None of that exists here. Completion is a function
// returning. The cap is a semaphore: a download acquires its own size before
// starting and releases it once the file is off local disk, so when the remote
// is slow the acquire blocks and downloads pause on their own.
//
// Blocking in the iterator is safe, but not for the reason it first appears.
// core's Download calls Iter.Next from its dispatch loop while workers run in an
// errgroup, so a blocked Next stalls new work without stopping the uploads that
// free the budget. What is *not* safe is reporting an error through Iter.Err:
// Download then returns without joining its workers (downloader.go:65-68), and
// tearing down the upload channel underneath them panics. So elemIter always
// reports a nil Err and stashes the real one, which Download's return
// guarantees is safe to read.
func Run(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o Options) (Result, error) {
	if o.Uploads <= 0 {
		o.Uploads = 1
	}
	if o.MaxFailures <= 0 {
		o.MaxFailures = 5
	}
	if o.Events == nil {
		o.Events = nopEvents{}
	}

	local, err := fs.NewFs(ctx, o.Staging)
	if err != nil {
		return Result{}, fmt.Errorf("open staging directory as a filesystem: %w", err)
	}
	up := &uploader{local: local, dst: o.Dst, confirm: o.Confirm}

	budget := newBudget(o.Budget)
	guard := newSpaceGuard(o.FreeBytes, o.MinFree)
	uploads := make(chan tgsource.Item, o.Uploads)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		errs    []error
		nErrs   int
		streak  int
		tripped bool
	)

	// stopDownloads ends the download side once the destination has stopped
	// accepting work. It stops the iterator rather than cancelling a context,
	// because cancelling only the uploads would leave downloads running at full
	// speed against a remote that is refusing them — every file staying on disk,
	// every reservation released on the way out. A broken remote would fill the
	// local disk faster than a working one does.
	stopDownloads := new(atomic.Bool)

	for range o.Uploads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range uploads {
				// A full destination is not worth another multi-gigabyte
				// attempt, so the file is dropped rather than uploaded.
				err := guard.check(ctx)
				if err == nil {
					o.Events.UploadStart(it)
					err = up.upload(ctx, it)
					o.Events.UploadDone(it, err)
				}

				if err != nil {
					// MoveFile leaves the local copy in place when it fails, so
					// the reservation cannot simply be handed back — the bytes
					// are still on disk. Removing the file first is what keeps
					// the cap honest.
					if rerr := os.Remove(filepath.Join(o.Staging, it.Name)); rerr != nil && !os.IsNotExist(rerr) {
						err = errors.Join(err, fmt.Errorf("and it is still in staging: %w", rerr))
					}
				}
				budget.release(it.Size())

				mu.Lock()
				if err != nil {
					if nErrs < maxRecordedErrors {
						errs = append(errs, err)
					}
					nErrs++
					streak++
					// A full remote trips immediately: unlike a transient
					// error, waiting for a streak just wastes the downloads.
					if (streak >= o.MaxFailures || errors.Is(err, ErrDestinationFailing)) && !tripped {
						tripped = true
						stopDownloads.Store(true)
					}
				} else {
					streak = 0
				}
				mu.Unlock()
			}
		}()
	}

	var (
		dlOutcomes []Outcome
		stats      Stats
		dlErrs     []error
		at         = make(map[int]int) // message id -> its place in dlOutcomes
	)
	pending := seq
	for attempt := 1; ; attempt++ {
		sourceDown := false
		passOutcomes, passStats, err := download(ctx, pending, DownloadOptions{
			Pool:        o.Pool,
			Staging:     o.Staging,
			Threads:     o.Threads,
			Limit:       o.Limit,
			Takeout:     o.Takeout,
			Events:      o.Events,
			Refresh:     o.Refresh,
			acquire:     budget.acquire,
			release:     budget.release,
			maxFailures: o.MaxFailures,
			seed:        stats,
			onReady:     func(it tgsource.Item) { uploads <- it },
			onFailed: func(it tgsource.Item) {
				// Nothing was staged, so the reservation has to come back here
				// instead of from an upload that will never happen.
				budget.release(it.Size())
			},
			stop:   stopDownloads,
			onTrip: func() { sourceDown = true },
		})
		stats = passStats
		if err != nil {
			dlErrs = append(dlErrs, err)
		}
		// One outcome per item, whatever it took: a later attempt replaces the
		// earlier verdict in place, so an item that failed once and then
		// arrived is reported as archived rather than as both.
		for _, oc := range passOutcomes {
			if i, ok := at[oc.Item.MessageID]; ok {
				dlOutcomes[i] = oc
				continue
			}
			at[oc.Item.MessageID] = len(dlOutcomes)
			dlOutcomes = append(dlOutcomes, oc)
		}

		// Read, and the shared stop flag cleared, under the upload leg's own
		// lock. The flag is what the download breaker sets to end a pass, so a
		// retry needs it clear — but clearing it after the upload breaker has
		// tripped in this same window would restart downloads into a
		// destination that has stopped accepting them, and that leg never sets
		// the flag twice.
		mu.Lock()
		destDown := tripped
		if !destDown {
			stopDownloads.Store(false)
		}
		mu.Unlock()

		// Another pass is worth making only for items that failed on their own.
		// A pass that returned an error failed as a whole — the walk broke, a
		// name could not be opened, the run was cancelled — and the items it
		// never reached are lost to this run either way, so retrying the few
		// that failed first would dress that up as a nearly complete run. A
		// destination that has stopped accepting uploads rules it out too:
		// fetching more would only fill staging with files it will refuse.
		again := failedItems(passOutcomes)
		if len(again) > 0 && attempt < downloadAttempts &&
			err == nil && !destDown && ctx.Err() == nil {
			// These items are being fetched again, so their first attempt comes
			// back out of the totals: one item is one file to fetch, not one per
			// attempt. Bytes already transferred stay counted — they were really
			// spent, and throughput is the honest figure.
			stats = rollback(stats, again)

			wait := retryDelay(attempt)
			o.Events.Retry(attempt+1, len(again), wait)
			if serr := sleep(ctx, wait); serr == nil {
				pending = itemsSeq(again)
				continue
			}
			dlErrs = append(dlErrs, fmt.Errorf("cancelled before retrying %d download(s): %w",
				len(again), ctx.Err()))
		}

		if sourceDown {
			dlErrs = append(dlErrs, fmt.Errorf("%w: gave up after %d consecutive download failure(s)",
				ErrSourceFailing, o.MaxFailures))
		}
		break
	}
	dlErr := errors.Join(dlErrs...)

	// Safe only because Download joined its workers, which is guaranteed by
	// elemIter.Err always being nil.
	close(uploads)
	wg.Wait()

	mu.Lock()
	joined := errors.Join(errs...)
	trip, total := tripped, nErrs
	mu.Unlock()

	// Both halves are reported. On the most common failure path — Ctrl-C — the
	// download side returns context.Canceled while the upload workers drain
	// whatever is still queued, fail every one of them against the cancelled
	// context, and delete the staged file each time. Returning only the download
	// error would leave the operator with "context canceled" and no sign that
	// finished files had been discarded.
	var upErr error
	switch {
	case trip:
		upErr = fmt.Errorf("%w: stopped after %d upload failure(s): %w",
			ErrDestinationFailing, total, joined)
	case total > 0:
		upErr = fmt.Errorf("%d upload(s) failed: %w", total, joined)
	}
	return Result{Stats: stats, Outcomes: dlOutcomes}, errors.Join(dlErr, upErr)
}

// failedItems lists the items a pass did not manage to fetch.
func failedItems(outcomes []Outcome) []tgsource.Item {
	var out []tgsource.Item
	for _, oc := range outcomes {
		if oc.Err != nil {
			out = append(out, oc.Item)
		}
	}
	return out
}

// rollback takes a pass's failures back out of the running totals so the next
// pass counts them once, not twice.
func rollback(s Stats, again []tgsource.Item) Stats {
	for _, it := range again {
		s.Started--
		s.Failed--
		s.BytesTotal -= it.Size()
	}
	return s
}

// itemsSeq feeds a retry pass the items the last one failed.
func itemsSeq(items []tgsource.Item) iter.Seq2[tgsource.Item, error] {
	return func(yield func(tgsource.Item, error) bool) {
		for _, it := range items {
			if !yield(it, nil) {
				return
			}
		}
	}
}

// budget bounds how many bytes of downloaded-but-not-yet-uploaded data sit on
// local disk. A zero limit means no bound.
type budget struct{ sem *semaphore.Weighted }

func newBudget(limit int64) *budget {
	if limit <= 0 {
		return &budget{}
	}
	return &budget{sem: semaphore.NewWeighted(limit)}
}

func (b *budget) acquire(ctx context.Context, n int64) error {
	if b.sem == nil {
		return nil
	}
	// An item larger than the budget cannot be admitted, and semaphore.Acquire
	// handles that by blocking until the context is cancelled rather than
	// failing — so there is no error to surface and no guard to add here. The
	// real protection is validateBudget refusing such a run before it starts.
	if err := b.sem.Acquire(ctx, n); err != nil {
		return fmt.Errorf("waiting for %d bytes of staging space: %w", n, err)
	}
	return nil
}

func (b *budget) release(n int64) {
	if b.sem != nil {
		b.sem.Release(n)
	}
}
