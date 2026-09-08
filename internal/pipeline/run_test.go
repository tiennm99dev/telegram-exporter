package pipeline

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// These exercise Run's composition rather than its parts. Everything underneath
// it is tested directly, but the properties Run alone owns — that the upload
// channel closes only after the last send, that reservations balance across a
// whole run, that a tripped breaker still terminates — only exist once the
// pieces are wired together, and a regression in any of them is silent.

// runFake substitutes the download step for the duration of a test.
//
// It also collapses the wait between retry passes. A run retries the items it
// failed to fetch, and at the real cadence every test with a failure in it would
// spend minutes asleep.
func runFake(t *testing.T, f func(context.Context, iter.Seq2[tgsource.Item, error], DownloadOptions) ([]Outcome, Stats, error)) {
	t.Helper()
	prev, prevDelay := download, retryDelay
	download = f
	retryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() {
		download, retryDelay = prev, prevDelay
	})
}

// stageItems is a download step that writes each item's bytes into staging and
// hands it to the upload leg, mimicking what the real one does on success.
// Items named in fail never reach staging.
func stageItems(fail map[int]bool) func(context.Context, iter.Seq2[tgsource.Item, error], DownloadOptions) ([]Outcome, Stats, error) {
	return func(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
		var outcomes []Outcome
		// Seeded exactly as the real step does, so a retry pass continues the
		// run's totals instead of restarting them.
		stats := o.seed
		for it, err := range seq {
			if err != nil {
				return outcomes, stats, err
			}
			if o.stop != nil && o.stop.Load() {
				break
			}
			if o.acquire != nil {
				if aerr := o.acquire(ctx, it.Size()); aerr != nil {
					return outcomes, stats, aerr
				}
			}
			stats.Started++
			if fail[it.MessageID] {
				stats.Failed++
				outcomes = append(outcomes, Outcome{Item: it, Err: errors.New("download failed")})
				o.onFailed(it)
				continue
			}
			if werr := os.WriteFile(filepath.Join(o.Staging, it.Name),
				make([]byte, it.Size()), 0o600); werr != nil {
				return outcomes, stats, werr
			}
			stats.Done++
			stats.BytesDone += it.Size()
			outcomes = append(outcomes, Outcome{Item: it})
			o.onReady(it)
		}
		return outcomes, stats, nil
	}
}

func testItems(n int, size int64) []tgsource.Item {
	out := make([]tgsource.Item, n)
	for i := range out {
		out[i] = tgsource.Item{
			MessageID: i + 1,
			Name:      fmt.Sprintf("-100123_%d_file.bin", i+1),
			Media:     &tmedia.Media{Size: size},
		}
	}
	return out
}

func seqOf(items []tgsource.Item) iter.Seq2[tgsource.Item, error] {
	return func(yield func(tgsource.Item, error) bool) {
		for _, it := range items {
			if !yield(it, nil) {
				return
			}
		}
	}
}

// runOpts wires Run against local directories, so uploads are real rclone moves.
func runOpts(t *testing.T, budget int64) (Options, string, string) {
	t.Helper()
	staging, dstDir := t.TempDir(), t.TempDir()
	dst, err := fs.NewFs(t.Context(), dstDir)
	if err != nil {
		t.Fatalf("open destination: %v", err)
	}
	return Options{
		Dst:     dst,
		Staging: staging,
		Uploads: 2,
		Budget:  budget,
		Confirm: true,
	}, staging, dstDir
}

func TestRunUploadsEveryDownloadedItem(t *testing.T) {
	runFake(t, stageItems(nil))
	o, staging, dstDir := runOpts(t, 0)
	items := testItems(6, 512)

	res, err := Run(t.Context(), seqOf(items), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stats.Done != len(items) {
		t.Errorf("Done = %d, want %d", res.Stats.Done, len(items))
	}
	for _, it := range items {
		info, serr := os.Stat(filepath.Join(dstDir, it.Name))
		if serr != nil {
			t.Errorf("%s not on the destination: %v", it.Name, serr)
			continue
		}
		if info.Size() != it.Size() {
			t.Errorf("%s is %d bytes, want %d", it.Name, info.Size(), it.Size())
		}
	}
	assertEmpty(t, staging)
}

// The reason Err always reports nil: if Run closed the upload channel before
// the download step finished handing over items, this panics.
func TestRunClosesUploadsOnlyAfterTheLastSend(t *testing.T) {
	var late atomic.Bool
	runFake(t, func(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
		// A worker still delivering after the step's own error is exactly what
		// core does when it skips wg.Wait, so send one and then fail.
		it := testItems(1, 128)[0]
		if err := os.WriteFile(filepath.Join(o.Staging, it.Name), make([]byte, it.Size()), 0o600); err != nil {
			return nil, Stats{}, err
		}
		o.acquire(ctx, it.Size())
		o.onReady(it)
		late.Store(true)
		return []Outcome{{Item: it}}, Stats{Started: 1, Done: 1}, errors.New("download step failed")
	})
	o, staging, dstDir := runOpts(t, 0)

	_, err := Run(t.Context(), seqOf(nil), o)
	if err == nil {
		t.Fatal("Run returned nil, want the download step's error")
	}
	if !late.Load() {
		t.Fatal("the download step never ran")
	}
	// The handed-over item must still have been uploaded, not dropped.
	if _, serr := os.Stat(filepath.Join(dstDir, "-100123_1_file.bin")); serr != nil {
		t.Errorf("item handed over before the error was not uploaded: %v", serr)
	}
	assertEmpty(t, staging)
}

// A reservation that is not returned shrinks the cap for the rest of the run,
// and one returned twice panics. Neither is visible in the pieces individually.
//
// The budget here is exactly one file, so the run can only proceed if every
// reservation comes back: the second item cannot start until the first is
// released. A leak deadlocks and this test times out rather than passing
// quietly, which is the whole point of sizing it this way.
func TestRunBalancesTheBudgetAcrossFailures(t *testing.T) {
	const size = 1024
	runFake(t, stageItems(map[int]bool{2: true, 5: true}))
	o, staging, dstDir := runOpts(t, size)
	items := testItems(8, size)

	res, err := Run(t.Context(), seqOf(items), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(res.Failed()); got != 2 {
		t.Errorf("Failed() = %d, want 2", got)
	}
	if res.Stats.Done != 6 {
		t.Errorf("Done = %d, want 6", res.Stats.Done)
	}
	// The six that downloaded are on the destination; the two that failed are
	// not, and neither is holding space.
	for _, it := range items {
		_, serr := os.Stat(filepath.Join(dstDir, it.Name))
		wantThere := it.MessageID != 2 && it.MessageID != 5
		if wantThere && serr != nil {
			t.Errorf("%s should be on the destination: %v", it.Name, serr)
		}
		if !wantThere && !os.IsNotExist(serr) {
			t.Errorf("%s should not be on the destination", it.Name)
		}
	}
	assertEmpty(t, staging)
}

func TestRunStopsDownloadingAfterConsecutiveUploadFailures(t *testing.T) {
	const size = 256
	items := testItems(40, size)

	var dispatched atomic.Int64
	runFake(t, func(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
		var outcomes []Outcome
		// Seeded exactly as the real step does, so a retry pass continues the
		// run's totals instead of restarting them.
		stats := o.seed
		for it := range seqValues(seq) {
			if o.stop.Load() {
				break
			}
			o.acquire(ctx, it.Size())
			dispatched.Add(1)
			// Nothing is written to staging, so every upload fails to find it.
			stats.Started++
			stats.Done++
			outcomes = append(outcomes, Outcome{Item: it})
			o.onReady(it)
		}
		return outcomes, stats, nil
	})
	o, staging, _ := runOpts(t, 0)
	o.MaxFailures = 3

	_, err := Run(t.Context(), seqOf(items), o)
	if err == nil {
		t.Fatal("Run returned nil, want the breaker's error")
	}
	// The sentinel, not the wording: the caller maps this to a distinct exit
	// code so a driver stops instead of retrying against a dead remote.
	if !errors.Is(err, ErrDestinationFailing) {
		t.Errorf("error is not ErrDestinationFailing: %v", err)
	}
	// The point of stopping the iterator rather than cancelling uploads: the
	// download side must not have walked the whole chat.
	if got := dispatched.Load(); got == int64(len(items)) {
		t.Errorf("all %d items were dispatched; the breaker did not stop downloads", got)
	}
	assertEmpty(t, staging)
}

func seqValues(seq iter.Seq2[tgsource.Item, error]) iter.Seq[tgsource.Item] {
	return func(yield func(tgsource.Item) bool) {
		for it, err := range seq {
			if err != nil {
				return
			}
			if !yield(it) {
				return
			}
		}
	}
}

// A failed upload must take the staged file with it, or the cap is over-committed
// by that much for the rest of the run.
func TestRunClearsStagingWhenAnUploadFails(t *testing.T) {
	const size = 512
	runFake(t, func(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
		it := testItems(1, size)[0]
		// Staged at the wrong size, so the confirm step rejects it.
		if err := os.WriteFile(filepath.Join(o.Staging, it.Name), make([]byte, size/2), 0o600); err != nil {
			return nil, Stats{}, err
		}
		o.acquire(ctx, it.Size())
		o.onReady(it)
		return []Outcome{{Item: it}}, Stats{Started: 1, Done: 1}, nil
	})
	o, staging, dstDir := runOpts(t, 0)

	_, err := Run(t.Context(), seqOf(nil), o)
	if err == nil {
		t.Fatal("Run returned nil, want the confirm failure")
	}
	assertEmpty(t, staging)
	// And the short object must not be left under the name verify matches.
	if _, serr := os.Stat(filepath.Join(dstDir, "-100123_1_file.bin")); !os.IsNotExist(serr) {
		t.Errorf("short object left on the destination: %v", serr)
	}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("staging is not empty: %v", names)
	}
}

// A failure that clears on a later pass has to be reported as an archived file,
// not as both a failure and a success. The whole point of retrying in-run is
// that the outage which cost these items is usually over minutes later, and the
// next sync would have to re-walk the chat and re-index the remote to find out.
func TestRunRetriesFailedDownloadsWithinTheRun(t *testing.T) {
	const size = 512
	items := testItems(3, size)

	var passes atomic.Int64
	runFake(t, func(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
		// The middle item fails on the first pass only, as a transient outage
		// looks from here.
		fail := map[int]bool{2: passes.Add(1) == 1}
		return stageItems(fail)(ctx, seq, o)
	})
	o, staging, dstDir := runOpts(t, 0)

	res, err := Run(t.Context(), seqOf(items), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if passes.Load() != 2 {
		t.Errorf("passes = %d, want 2 — the failure was not retried", passes.Load())
	}
	if got := len(res.Failed()); got != 0 {
		t.Errorf("Failed() = %d, want 0: %v", got, res.Failed())
	}
	if len(res.Outcomes) != len(items) {
		t.Errorf("Outcomes = %d, want %d — a retried item must not be reported twice",
			len(res.Outcomes), len(items))
	}
	// Counters follow the same rule: an item is one file to fetch however many
	// attempts it took, or the closing summary claims more work than the chat
	// contains.
	if res.Stats.Done != 3 || res.Stats.Failed != 0 || res.Stats.Started != 3 {
		t.Errorf("Stats = %+v, want 3 started, 3 done, 0 failed", res.Stats)
	}
	for _, it := range items {
		if _, serr := os.Stat(filepath.Join(dstDir, it.Name)); serr != nil {
			t.Errorf("%s should be on the destination: %v", it.Name, serr)
		}
	}
	assertEmpty(t, staging)
}

// Retrying is bounded. A source that is down stays down for the run, and the
// verdict has to be the distinct sentinel: a driver looping on "incomplete"
// would otherwise re-walk the whole chat forever against a dead source.
func TestRunGivesUpAndNamesTheSourceAfterEveryPassFails(t *testing.T) {
	const size = 512
	items := testItems(2, size)

	var passes atomic.Int64
	runFake(t, func(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
		passes.Add(1)
		outcomes, stats, err := stageItems(map[int]bool{1: true, 2: true})(ctx, seq, o)
		// What Download reports once its own breaker has ended the pass.
		if o.onTrip != nil {
			o.onTrip()
		}
		return outcomes, stats, err
	})
	o, staging, _ := runOpts(t, 0)
	o.MaxFailures = 2

	res, err := Run(t.Context(), seqOf(items), o)
	if !errors.Is(err, ErrSourceFailing) {
		t.Fatalf("Run = %v, want ErrSourceFailing", err)
	}
	if got := passes.Load(); got != downloadAttempts {
		t.Errorf("passes = %d, want %d", got, downloadAttempts)
	}
	if got := len(res.Failed()); got != len(items) {
		t.Errorf("Failed() = %d, want %d", got, len(items))
	}
	assertEmpty(t, staging)
}

// A trip that the retry then clears must not reach the caller. Reported anyway,
// it would send a driver to the "source is down, stop" exit code on a run that
// finished everything.
func TestRunDoesNotReportASourceThatRecovered(t *testing.T) {
	const size = 512
	items := testItems(2, size)

	var passes atomic.Int64
	runFake(t, func(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
		first := passes.Add(1) == 1
		outcomes, stats, err := stageItems(map[int]bool{1: first, 2: first})(ctx, seq, o)
		if first && o.onTrip != nil {
			o.onTrip()
		}
		return outcomes, stats, err
	})
	o, _, _ := runOpts(t, 0)
	o.MaxFailures = 2

	res, err := Run(t.Context(), seqOf(items), o)
	if err != nil {
		t.Fatalf("Run = %v, want nil after the retry succeeded", err)
	}
	if got := len(res.Failed()); got != 0 {
		t.Errorf("Failed() = %d, want 0", got)
	}
}

// Every pass gets the refresher, retries included. Run feeds a failed item back
// through Download as the same struct it failed with, so without this a
// reference that expired mid-transfer would be replayed dead on every attempt.
func TestRunRefreshesOnEveryPassIncludingRetries(t *testing.T) {
	var passes int
	refresh := func(_ context.Context, it tgsource.Item) (tgsource.Item, error) { return it, nil }

	runFake(t, func(ctx context.Context, seq iter.Seq2[tgsource.Item, error], o DownloadOptions) ([]Outcome, Stats, error) {
		passes++
		if o.Refresh == nil {
			t.Errorf("pass %d was given no refresher", passes)
		}
		return stageItems(map[int]bool{2: true})(ctx, seq, o)
	})

	o, staging, _ := runOpts(t, 0)
	o.Refresh = refresh

	res, err := Run(t.Context(), seqOf(testItems(2, 10)), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if passes != downloadAttempts {
		t.Fatalf("passes = %d, want %d — the retry passes are where a stale reference would be replayed",
			passes, downloadAttempts)
	}
	if got := len(res.Failed()); got != 1 {
		t.Errorf("Failed() = %d, want 1", got)
	}
	assertEmpty(t, staging)
}
