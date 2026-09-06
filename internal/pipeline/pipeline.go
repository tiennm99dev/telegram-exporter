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

	Report func(Stats)
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

	local, err := fs.NewFs(ctx, o.Staging)
	if err != nil {
		return Result{}, fmt.Errorf("open staging directory as a filesystem: %w", err)
	}
	up := &uploader{local: local, dst: o.Dst, confirm: o.Confirm}

	budget := newBudget(o.Budget)
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
				err := up.upload(ctx, it)

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
					if streak >= o.MaxFailures && !tripped {
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

	dlOutcomes, stats, dlErr := download(ctx, seq, DownloadOptions{
		Pool:    o.Pool,
		Staging: o.Staging,
		Threads: o.Threads,
		Limit:   o.Limit,
		Takeout: o.Takeout,
		Report:  o.Report,
		acquire: budget.acquire,
		release: budget.release,
		onReady: func(it tgsource.Item) { uploads <- it },
		onFailed: func(it tgsource.Item) {
			// Nothing was staged, so the reservation has to come back here
			// instead of from an upload that will never happen.
			budget.release(it.Size())
		},
		stop: stopDownloads,
	})

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
		upErr = fmt.Errorf("stopped after %d consecutive upload failures (%d total): %w",
			o.MaxFailures, total, joined)
	case total > 0:
		upErr = fmt.Errorf("%d upload(s) failed: %w", total, joined)
	}
	return Result{Stats: stats, Outcomes: dlOutcomes}, errors.Join(dlErr, upErr)
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
