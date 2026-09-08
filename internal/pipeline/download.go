package pipeline

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/downloader"

	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// DownloadOptions configures a download run.
type DownloadOptions struct {
	Pool    dcpool.Pool
	Staging string
	Threads int  // connections per file
	Limit   int  // files in flight
	Takeout bool // use a takeout session, as `tdl dl --takeout` does

	// Events, when set, receives the run's per-item lifecycle. It is invoked
	// from download worker goroutines, so it must be cheap and safe to call
	// concurrently.
	Events Events

	// acquire reserves staging space before a download starts, blocking until
	// there is room. Unset means no bound. release hands a reservation back for
	// an item that never reaches a download.
	acquire func(context.Context, int64) error
	release func(int64)

	// stop, when set, ends iteration cleanly from another goroutine — used to
	// halt downloads once the destination has stopped accepting uploads.
	stop *atomic.Bool
	// maxFailures ends the pass after this many consecutive failures. Zero means
	// no breaker. onTrip, when set, is called once if that happens — the caller
	// decides what an abandoned pass means, because a retry that then succeeds
	// must not report the source as down.
	maxFailures int
	onTrip      func()
	// seed starts the run's counters from an earlier pass's totals, so a retry
	// continues the numbers a reporter is already displaying rather than
	// restarting them at zero.
	seed Stats
	// onReady hands a completed file to the upload leg; onFailed says nothing
	// was staged, so whatever acquire reserved must be given back.
	onReady  func(tgsource.Item)
	onFailed func(tgsource.Item)
}

// Download fetches every item in seq into the staging directory.
//
// Each file is written to <name>.part and renamed to <name> only once its size
// matches what Telegram reported, so a name without the suffix is always a whole
// file. Uploads are driven by completion rather than by scanning for that, but
// the invariant still matters: it is what makes a leftover file from an
// interrupted run safe to keep and a leftover .part safe to delete. The shell
// pipeline could only approximate it with a filename convention plus an age
// guard, because it could not see inside tdl.
//
// A failed item does not abort the run: it is recorded in the returned outcomes
// and the rest continue, matching what a partial `tdl dl` pass did. Past
// maxFailures failures in a row the pass gives up on the remaining items and
// calls onTrip, because a source that is refusing every file will refuse the
// rest of the list too, in milliseconds, and a pass with no brake spends the
// whole todo list proving it.
func Download(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
	if o.Threads <= 0 {
		o.Threads = 4
	}
	if o.Limit <= 0 {
		o.Limit = 2
	}
	if err := os.MkdirAll(o.Staging, 0o755); err != nil {
		return nil, Stats{}, fmt.Errorf("create staging directory: %w", err)
	}

	// core's downloader logs why a transfer failed rather than returning it, so
	// the logger it reaches for is pointed back into this package before any of
	// its work starts. Without this the reason goes to a nop logger.
	ctx = captureCauses(ctx)

	it := newElemIter(seq, o.Staging, o.Takeout)
	it.acquire = o.acquire
	it.release = o.release
	if o.stop != nil {
		it.stopped = o.stop
	}
	defer func() { _ = it.Close() }()

	prog := newProgress(func(e *elem, err error) error {
		ferr := finish(o.Staging, e, err)
		if err == nil && ferr == nil {
			if o.onReady != nil {
				o.onReady(e.item)
			}
			return nil
		}
		if o.onFailed != nil {
			o.onFailed(e.item)
		}
		return ferr
	}, o.Events)
	// Set rather than passed: both are this package's own bookkeeping, and no
	// worker exists yet, so the mutex the fields normally live under has
	// nothing to protect them from here.
	prog.stats = o.seed
	prog.maxStreak = o.maxFailures
	prog.onTrip = func() { it.stopped.Store(true) }

	err := downloader.New(downloader.Options{
		Pool:     o.Pool,
		Threads:  o.Threads,
		Iter:     it,
		Progress: prog,
	}).Download(ctx, o.Limit)

	outcomes, stats := prog.results()
	// The iterator's failure is read only now, after Download has joined every
	// worker. Reporting it through Iter.Err would have made Download skip that
	// join entirely.
	if err == nil {
		err = it.failure
	}
	// A tripped breaker is not an error here. It is reported to the caller, who
	// knows whether another pass is coming; the items the pass never reached are
	// simply absent from the outcomes.
	if prog.brokeCircuit() && o.onTrip != nil {
		o.onTrip()
	}
	// Skipped items are reported alongside whatever else happened rather than
	// instead of it: the run did real work, and the caller still needs to know
	// these messages were never attempted.
	return outcomes, stats, errors.Join(append([]error{err}, it.skipped...)...)
}

// finish closes a downloaded file and either promotes it or removes it.
//
// The size on disk is checked against the size Telegram reported, and that check
// is not belt-and-braces — it is the only reliable failure signal available.
// core's Download swallows non-cancellation errors: it logs them and returns
// nil, and OnDone is deferred on that named return, so a failed transfer arrives
// here indistinguishable from a successful one (downloader.go:47-60). Trusting
// the error alone would promote a truncated file to its final name, and the
// upload leg would archive it as complete.
//
// A partial file is deleted rather than kept: the downloader exposes no resume
// offset, so a leftover .part could never be continued, and leaving one behind
// would only invite a later run to mistake it for progress.
func finish(staging string, e *elem, downloadErr error) error {
	part := partPath(staging, e.item)

	if cerr := e.file.Close(); cerr != nil && downloadErr == nil {
		downloadErr = cerr
	}

	if downloadErr == nil {
		if err := checkSize(part, e.item.Size()); err != nil {
			// The size is the symptom. e.cause is the reason, when the
			// downloader logged one, and it is put first: "got 0 bytes" alone
			// says a two-gigabyte fetch failed without saying anything an
			// operator can act on.
			if e.cause != nil {
				err = fmt.Errorf("%w: %w", e.cause, err)
			}
			downloadErr = err
		}
	}

	if downloadErr != nil {
		if rerr := os.Remove(part); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("remove partial %q: %w", part, rerr)
		}
		// Returned so the caller records a failure even when the downloader
		// claimed success; otherwise a short file would vanish silently and the
		// run would report itself complete.
		return downloadErr
	}

	if err := os.Rename(part, finalPath(staging, e.item)); err != nil {
		// The part file goes too. The caller treats this as a failure and hands
		// the byte reservation back, so leaving the file on disk would put the
		// staging cap permanently over-committed by its size.
		if rerr := os.Remove(part); rerr != nil && !os.IsNotExist(rerr) {
			return errors.Join(fmt.Errorf("promote %q: %w", part, err),
				fmt.Errorf("and it is still in staging: %w", rerr))
		}
		return fmt.Errorf("promote %q: %w", part, err)
	}
	return nil
}

// checkSize compares what landed on disk against what Telegram said the file is.
func checkSize(path string, want int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat downloaded file: %w", err)
	}
	if info.Size() != want {
		return fmt.Errorf("short download: got %d bytes, expected %d", info.Size(), want)
	}
	return nil
}

// SweepPartials removes leftover .part files from an earlier run.
//
// They cannot be resumed — core's downloader takes no starting offset — so the
// only options are delete or accumulate, and accumulating fills the disk with
// fragments no run will ever finish.
func SweepPartials(staging string) (int, error) {
	entries, err := os.ReadDir(staging)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read staging directory: %w", err)
	}

	removed := 0
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), partSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(staging, entry.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
