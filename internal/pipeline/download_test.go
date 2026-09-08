package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/downloader"
	"github.com/iyear/tdl/core/logctx"
	"github.com/iyear/tdl/core/tmedia"
	"go.uber.org/zap"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

func testItem(t *testing.T, msgID int, file string, size int64) tgsource.Item {
	t.Helper()
	m := &tmedia.Media{
		Name:         file,
		Size:         size,
		DC:           2,
		InputFileLoc: &tg.InputDocumentFileLocation{ID: int64(msgID)},
	}
	return tgsource.Item{
		DialogID:  1234567890,
		MessageID: msgID,
		Name:      naming.For(1234567890, msgID, m),
		Media:     m,
	}
}

// openElem mimics what elemIter does, so finish can be tested without a network.
func openElem(t *testing.T, staging string, it tgsource.Item) *elem {
	t.Helper()
	f, err := os.OpenFile(partPath(staging, it), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open part file: %v", err)
	}
	return &elem{item: it, file: f}
}

// A name without the suffix must always be a whole file: that is the property
// the upload half relies on to treat "exists" as "finished", replacing the
// filename-convention-plus-age-guard the shell pipeline needed.
func TestFinishPromotesOnlyOnSuccess(t *testing.T) {
	staging := t.TempDir()
	it := testItem(t, 1, "video.mp4", 100)

	e := openElem(t, staging, it)
	if _, err := e.file.WriteAt(make([]byte, it.Size()), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := finish(staging, e, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if _, err := os.Stat(finalPath(staging, it)); err != nil {
		t.Errorf("final file missing after a successful download: %v", err)
	}
	if _, err := os.Stat(partPath(staging, it)); !os.IsNotExist(err) {
		t.Errorf("part file still present after promotion")
	}
}

func TestFinishRemovesPartialOnFailure(t *testing.T) {
	staging := t.TempDir()
	it := testItem(t, 2, "video.mp4", 100)

	e := openElem(t, staging, it)
	if _, err := e.file.WriteAt([]byte("half"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	// finish returns the failure rather than swallowing it, so the caller
	// records the item as failed instead of quietly counting it done.
	want := errors.New("connection reset")
	if err := finish(staging, e, want); !errors.Is(err, want) {
		t.Fatalf("finish = %v, want the download error returned", err)
	}

	// Neither file may survive: a partial promoted to the final name would be
	// indistinguishable from a complete download and would never be repaired.
	if _, err := os.Stat(partPath(staging, it)); !os.IsNotExist(err) {
		t.Errorf("part file survived a failed download")
	}
	if _, err := os.Stat(finalPath(staging, it)); !os.IsNotExist(err) {
		t.Errorf("a failed download was promoted to the final name")
	}
}

func TestSweepPartialsRemovesOnlyPartFiles(t *testing.T) {
	staging := t.TempDir()
	keep := filepath.Join(staging, "1234567890_1_done.mp4")
	drop := filepath.Join(staging, "1234567890_2_wip.mp4"+partSuffix)
	legacy := filepath.Join(staging, "1234567890_3_old.mp4.tmp")

	for _, p := range []string{keep, drop, legacy} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %q: %v", p, err)
		}
	}

	removed, err := SweepPartials(staging)
	if err != nil {
		t.Fatalf("SweepPartials: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a completed file was swept: %v", err)
	}
	// tdl's own suffix is left alone: a shared staging directory during the
	// cutover may hold files a legacy run is still writing.
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("a legacy tdl .tmp file was swept: %v", err)
	}
}

func TestSweepPartialsOnMissingDirectory(t *testing.T) {
	removed, err := SweepPartials(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Errorf("SweepPartials on a missing directory = %v, want nil", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

// An unwritable name must be refused with a message naming the message, rather
// than surfacing as a bare os.Create failure later — and it must be skipped, not
// treated as the end of the walk. One hostile filename cannot be allowed to
// strand every message behind it.
func TestElemIterSkipsUnsafeNamesAndKeepsGoing(t *testing.T) {
	staging := t.TempDir()
	bad := testItem(t, 7, "../../escape.conf", 10)
	good := testItem(t, 8, "fine.mp4", 10)

	seq := func(yield func(tgsource.Item, error) bool) {
		if !yield(bad, nil) {
			return
		}
		yield(good, nil)
	}
	it := newElemIter(seq, staging, false)
	defer func() { _ = it.Close() }()

	if !it.Next(t.Context()) {
		t.Fatal("an unsafe name ended the walk; the item after it was never reached")
	}
	if got := it.current.item.MessageID; got != 8 {
		t.Fatalf("Next yielded message %d, want the item after the unsafe one", got)
	}
	if it.Next(t.Context()) {
		t.Error("iterator produced a third item")
	}
	if it.failure != nil {
		t.Errorf("a skipped item must not fail the run, got: %v", it.failure)
	}
	if len(it.skipped) != 1 {
		t.Fatalf("skipped = %d, want 1", len(it.skipped))
	}
	if !strings.Contains(it.skipped[0].Error(), "message 7") {
		t.Errorf("the skip should name the message, got: %v", it.skipped[0])
	}
}

// The skipped items still have to reach the caller: the run did work, but these
// messages were never attempted and nothing else would say so.
func TestDownloadReportsSkippedItems(t *testing.T) {
	staging := t.TempDir()
	bad := testItem(t, 7, "../../escape.conf", 10)

	seq := func(yield func(tgsource.Item, error) bool) { yield(bad, nil) }
	it := newElemIter(seq, staging, false)
	defer func() { _ = it.Close() }()

	for it.Next(t.Context()) {
	}
	err := errors.Join(append([]error{nil}, it.skipped...)...)
	if err == nil {
		t.Fatal("skipped items produced no error for the caller")
	}
	if !strings.Contains(err.Error(), "message 7") {
		t.Errorf("error should name the skipped message, got: %v", err)
	}
}

func TestElemIterOpensPartFilesAndPropagatesWalkErrors(t *testing.T) {
	staging := t.TempDir()
	good := testItem(t, 1, "a.mp4", 10)

	t.Run("opens a part file", func(t *testing.T) {
		seq := func(yield func(tgsource.Item, error) bool) { yield(good, nil) }
		it := newElemIter(seq, staging, true)
		defer func() { _ = it.Close() }()

		if !it.Next(t.Context()) {
			t.Fatalf("Next() = false, Err() = %v", it.Err())
		}
		e := it.Value()
		if !e.AsTakeout() {
			t.Error("AsTakeout() = false, want the configured value")
		}
		if e.File().Size() != 10 || e.File().DC() != 2 {
			t.Errorf("File() = size %d dc %d, want 10 and 2", e.File().Size(), e.File().DC())
		}
		if _, err := os.Stat(partPath(staging, good)); err != nil {
			t.Errorf("part file was not created: %v", err)
		}
	})

	t.Run("propagates a walk error", func(t *testing.T) {
		want := errors.New("history walk failed")
		seq := func(yield func(tgsource.Item, error) bool) { yield(tgsource.Item{}, want) }
		it := newElemIter(seq, staging, false)
		defer func() { _ = it.Close() }()

		if it.Next(t.Context()) {
			t.Fatal("Next() = true after a walk error")
		}
		if !errors.Is(it.failure, want) {
			t.Errorf("failure = %v, want %v", it.failure, want)
		}
	})
}

// Byte accounting has to treat ProgressState as a running total, not a delta,
// or the aggregate drifts upward on every callback.
func TestProgressAccountsBytesAsRunningTotals(t *testing.T) {
	staging := t.TempDir()
	it := testItem(t, 1, "a.mp4", 100)
	e := openElem(t, staging, it)

	p := newProgress(func(*elem, error) error { return nil }, nil)
	p.OnAdd(e)
	p.OnDownload(e, progressState(40))
	p.OnDownload(e, progressState(100))
	p.OnDone(e, nil)

	outcomes, stats := p.results()
	if stats.BytesDone != 100 {
		t.Errorf("BytesDone = %d, want 100 (states are totals, not deltas)", stats.BytesDone)
	}
	if stats.BytesTotal != 100 {
		t.Errorf("BytesTotal = %d, want 100", stats.BytesTotal)
	}
	if stats.Done != 1 || stats.Failed != 0 {
		t.Errorf("Done/Failed = %d/%d, want 1/0", stats.Done, stats.Failed)
	}
	if len(outcomes) != 1 || outcomes[0].Err != nil {
		t.Errorf("outcomes = %+v, want one success", outcomes)
	}
}

func TestProgressRecordsFailuresWithoutAborting(t *testing.T) {
	staging := t.TempDir()
	ok := testItem(t, 1, "a.mp4", 10)
	bad := testItem(t, 2, "b.mp4", 10)

	p := newProgress(func(*elem, error) error { return nil }, nil)
	p.OnDone(openElem(t, staging, ok), nil)
	p.OnDone(openElem(t, staging, bad), errors.New("flood wait"))

	outcomes, stats := p.results()
	if stats.Done != 1 || stats.Failed != 1 {
		t.Errorf("Done/Failed = %d/%d, want 1/1", stats.Done, stats.Failed)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2 — a failure must be recorded, not dropped", len(outcomes))
	}
}

func progressState(done int64) downloader.ProgressState {
	return downloader.ProgressState{Downloaded: done, Total: 100}
}

// core's Download logs a failed transfer and returns nil, and OnDone is deferred
// on that named return — so a truncated file reaches finish claiming success.
// The size check is the only thing standing between that and an archived
// fragment, so it is tested directly.
func TestFinishRejectsShortDownloadDespiteNilError(t *testing.T) {
	staging := t.TempDir()
	it := testItem(t, 3, "video.mp4", 1000)

	e := openElem(t, staging, it)
	if _, err := e.file.WriteAt(make([]byte, 400), 0); err != nil {
		t.Fatalf("write: %v", err)
	}

	// nil, exactly as the downloader reports a failed transfer.
	err := finish(staging, e, nil)
	if err == nil {
		t.Fatal("finish accepted a 400-byte file for a 1000-byte item")
	}
	if !strings.Contains(err.Error(), "short download") {
		t.Errorf("error should name the short download, got: %v", err)
	}
	if _, err := os.Stat(finalPath(staging, it)); !os.IsNotExist(err) {
		t.Error("a truncated file was promoted to its final name")
	}
	if _, err := os.Stat(partPath(staging, it)); !os.IsNotExist(err) {
		t.Error("the truncated part file was left behind")
	}
}

// The size check must not reject a genuinely complete file.
func TestFinishAcceptsExactSize(t *testing.T) {
	staging := t.TempDir()
	it := testItem(t, 4, "exact.mp4", 2048)

	e := openElem(t, staging, it)
	if _, err := e.file.WriteAt(make([]byte, 2048), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := finish(staging, e, nil); err != nil {
		t.Fatalf("finish rejected an exact-size file: %v", err)
	}
	info, err := os.Stat(finalPath(staging, it))
	if err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	if info.Size() != 2048 {
		t.Errorf("final size = %d, want 2048", info.Size())
	}
}

// Err must always report nil, however badly iteration went.
//
// core's Download skips wg.Wait entirely when Iter.Err is non-nil
// (downloader.go:65-68), returning while its workers are still running. The
// pipeline closes its upload channel as soon as Download returns, so a non-nil
// Err here means workers send on a closed channel and the process panics —
// on every Ctrl-C, since cancellation is one of the ways iteration stops.
func TestElemIterNeverReportsErrToTheDownloader(t *testing.T) {
	staging := t.TempDir()

	cases := map[string]func() *elemIter{
		"walk error": func() *elemIter {
			seq := func(yield func(tgsource.Item, error) bool) {
				yield(tgsource.Item{}, errors.New("boom"))
			}
			return newElemIter(seq, staging, false)
		},
		"cancelled": func() *elemIter {
			seq := func(yield func(tgsource.Item, error) bool) {
				yield(testItem(t, 2, "a.mp4", 10), nil)
			}
			return newElemIter(seq, staging, false)
		},
	}

	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			it := build()
			defer func() { _ = it.Close() }()

			ctx := t.Context()
			if name == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}

			for it.Next(ctx) {
			}
			if err := it.Err(); err != nil {
				t.Errorf("Err() = %v, want nil — a non-nil Err makes Download abandon its workers", err)
			}
			if it.failure == nil {
				t.Error("the real failure was not stashed")
			}
		})
	}
}

// A caller-set stop flag ends iteration without looking like a failure, which is
// how the circuit breaker halts downloads.
func TestElemIterStopsOnFlagWithoutRecordingFailure(t *testing.T) {
	staging := t.TempDir()
	seq := func(yield func(tgsource.Item, error) bool) {
		for i := 1; i <= 5; i++ {
			if !yield(testItem(t, i, "a.mp4", 10), nil) {
				return
			}
		}
	}
	it := newElemIter(seq, staging, false)
	defer func() { _ = it.Close() }()

	if !it.Next(t.Context()) {
		t.Fatal("first Next() = false")
	}
	it.stopped.Store(true)

	if it.Next(t.Context()) {
		t.Error("Next() = true after the stop flag was set")
	}
	if it.failure != nil {
		t.Errorf("failure = %v, want nil — stopping is not a failure", it.failure)
	}
}

// A reservation must come back when the item never reaches a download, or the
// budget shrinks by that much for the rest of the run.
func TestElemIterReturnsReservationWhenOpenFails(t *testing.T) {
	// A staging path that is a file, not a directory, makes OpenFile fail.
	staging := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(staging, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	seq := func(yield func(tgsource.Item, error) bool) {
		yield(testItem(t, 1, "a.mp4", 4096), nil)
	}
	it := newElemIter(seq, staging, false)
	defer func() { _ = it.Close() }()

	var acquired, released int64
	it.acquire = func(_ context.Context, n int64) error { acquired += n; return nil }
	it.release = func(n int64) { released += n }

	if it.Next(t.Context()) {
		t.Fatal("Next() succeeded with an unusable staging directory")
	}
	if acquired != released {
		t.Errorf("acquired %d bytes but released %d — the reservation leaked", acquired, released)
	}
}

// The reason a transfer failed only exists in core's log call, so the capture is
// tested through that exact call rather than by poking the field directly: it is
// the shape of the log entry — a reflected element and a zap error — that this
// depends on, and a change to it must fail here rather than silently go back to
// reporting a byte count with no reason.
func TestFinishReportsWhyTheTransferFailed(t *testing.T) {
	staging := t.TempDir()
	it := testItem(t, 5, "video.mp4", 1000)
	e := openElem(t, staging, it)

	want := errors.New("rpc error code 420: FLOOD_WAIT (60)")
	ctx := captureCauses(t.Context())
	logctx.From(ctx).Error("Download error",
		zap.Any("element", downloader.Elem(e)), zap.Error(want))

	// nil, exactly as core reports a transfer it has already logged and given up
	// on, with nothing written to the file.
	err := finish(staging, e, nil)
	if !errors.Is(err, want) {
		t.Fatalf("finish = %v, want the logged transfer error", err)
	}
	if !strings.Contains(err.Error(), "expected 1000") {
		t.Errorf("the byte count must survive alongside the reason, got: %v", err)
	}
}

// Below error level nothing is captured, so an ordinary debug entry cannot
// attach a bogus reason to an item that merely arrived short.
func TestCaptureIgnoresNonErrorLogging(t *testing.T) {
	staging := t.TempDir()
	it := testItem(t, 6, "video.mp4", 1000)
	e := openElem(t, staging, it)

	ctx := captureCauses(t.Context())
	logctx.From(ctx).Debug("Start download elem", zap.Any("elem", downloader.Elem(e)))

	if e.cause != nil {
		t.Fatalf("cause = %v, want nil for a debug entry", e.cause)
	}
	if err := finish(staging, e, nil); !strings.Contains(err.Error(), "short download") {
		t.Errorf("finish = %v, want the plain size failure", err)
	}
}

// The breaker exists because a source that has stopped serving files fails every
// item in milliseconds: without it a pass spends the entire remaining todo list
// proving the same point. A success in between is what tells a run of bad luck
// apart from a source that is down, so it resets the streak.
func TestProgressBreakerTripsOnConsecutiveFailuresOnly(t *testing.T) {
	staging := t.TempDir()
	trips := 0

	p := newProgress(func(*elem, error) error { return nil }, nil)
	p.maxStreak = 3
	p.onTrip = func() { trips++ }

	fail := func(id int) { p.OnDone(openElem(t, staging, testItem(t, id, "a.mp4", 10)), errors.New("boom")) }
	ok := func(id int) { p.OnDone(openElem(t, staging, testItem(t, id, "a.mp4", 10)), nil) }

	fail(1)
	fail(2)
	ok(3) // the streak is broken here, so the next two must not trip it
	fail(4)
	fail(5)
	if trips != 0 {
		t.Fatalf("tripped after a success reset the streak (trips = %d)", trips)
	}
	if p.brokeCircuit() {
		t.Fatal("brokeCircuit() = true before the limit was reached")
	}

	fail(6)
	if trips != 1 {
		t.Fatalf("trips = %d, want 1 at three failures in a row", trips)
	}
	if !p.brokeCircuit() {
		t.Error("brokeCircuit() = false after the breaker fired")
	}

	// Fires once: the callback stops the iterator, and repeating it for every
	// item still in flight would be noise.
	fail(7)
	if trips != 1 {
		t.Errorf("trips = %d, want the breaker to fire exactly once", trips)
	}
}

// With no limit set there is no breaker at all, which is what a standalone
// Download must keep doing: every item gets an attempt.
func TestProgressWithoutABreakerNeverTrips(t *testing.T) {
	staging := t.TempDir()
	p := newProgress(func(*elem, error) error { return nil }, nil)
	p.onTrip = func() { t.Error("onTrip called with no limit configured") }

	for id := 1; id <= 20; id++ {
		p.OnDone(openElem(t, staging, testItem(t, id, "a.mp4", 10)), errors.New("boom"))
	}
	if p.brokeCircuit() {
		t.Error("brokeCircuit() = true with no limit configured")
	}
}
