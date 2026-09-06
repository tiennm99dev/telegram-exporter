package pipeline

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"

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

// Run downloads every item and uploads each one as it completes.
//
// This is the whole reason for the rewrite. run.sh could not see inside tdl, so
// it inferred completion from a filename suffix plus a file's age, polled
// `du -sk` every ten seconds, and enforced its disk cap by sending SIGSTOP and
// SIGCONT to the tdl process. None of that exists here. Completion is a function
// returning. The cap is a semaphore: a download acquires its own size before
// starting and releases it only once the upload has confirmed, so when the
// remote is slow the acquire blocks and downloads pause on their own.
//
// Blocking in the iterator is safe by construction — core's Download calls
// Iter.Next from its dispatch loop while workers run in an errgroup, so a
// blocked Next stalls new work without stopping the uploads that free the budget
// (downloader.go:36-63).
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

	// Upload workers own the release side of the budget, so every path out of
	// one — success, failure, cancellation — must release, or the run deadlocks
	// with downloads waiting on space that is never freed.
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		uploadErrs []error
		streak     int
		tripped    bool
	)
	upCtx, tripRun := context.WithCancel(ctx)
	defer tripRun()

	for range o.Uploads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range uploads {
				err := up.upload(upCtx, it)
				budget.release(it.Size())

				mu.Lock()
				if err != nil {
					uploadErrs = append(uploadErrs, err)
					streak++
					if streak >= o.MaxFailures && !tripped {
						// A remote that fails this many times running is not
						// going to recover on its own, and continuing just fills
						// staging until the disk does.
						tripped = true
						tripRun()
					}
				} else {
					streak = 0
				}
				mu.Unlock()
			}
		}()
	}

	dlOutcomes, stats, dlErr := Download(ctx, seq, DownloadOptions{
		Pool:    o.Pool,
		Staging: o.Staging,
		Threads: o.Threads,
		Limit:   o.Limit,
		Takeout: o.Takeout,
		Report:  o.Report,
		acquire: budget.acquire,
		onReady: func(it tgsource.Item) { uploads <- it },
		onFailed: func(it tgsource.Item) {
			// Nothing was staged, so the reservation has to come back here
			// instead of from an upload that will never happen.
			budget.release(it.Size())
		},
	})

	close(uploads)
	wg.Wait()

	mu.Lock()
	errs := append([]error(nil), uploadErrs...)
	trip := tripped
	mu.Unlock()

	res := Result{Stats: stats, Outcomes: dlOutcomes}
	switch {
	case dlErr != nil:
		return res, dlErr
	case trip:
		return res, fmt.Errorf("stopping after %d consecutive upload failures: %w",
			o.MaxFailures, errors.Join(errs...))
	case len(errs) > 0:
		return res, errors.Join(errs...)
	}
	return res, nil
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
	// An item larger than the whole budget could never be admitted and would
	// block forever, so it is refused with an error that says what to change.
	// Callers validate up front too; this is the guard for an item whose size
	// was not known then.
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
