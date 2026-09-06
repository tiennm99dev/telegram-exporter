package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/downloader"
	"github.com/iyear/tdl/core/tmedia"

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

// An unwritable name must stop the iterator with a message naming the message,
// rather than surfacing as a bare os.Create failure later.
func TestElemIterRejectsUnsafeNames(t *testing.T) {
	staging := t.TempDir()
	bad := testItem(t, 7, "../../escape.conf", 10)

	seq := func(yield func(tgsource.Item, error) bool) { yield(bad, nil) }
	it := newElemIter(seq, staging, false)
	defer func() { _ = it.Close() }()

	if it.Next(t.Context()) {
		t.Fatal("iterator accepted a name that escapes the staging directory")
	}
	err := it.Err()
	if err == nil {
		t.Fatal("Err() = nil after rejecting an unsafe name")
	}
	if !strings.Contains(err.Error(), "message 7") {
		t.Errorf("error should name the message, got: %v", err)
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
		if !errors.Is(it.Err(), want) {
			t.Errorf("Err() = %v, want %v", it.Err(), want)
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
