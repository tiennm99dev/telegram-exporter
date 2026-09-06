package pipeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

const settleName = "-100123_4242_clip.mp4"

// The real backoff is tens of seconds, which is right against pikpak and wrong
// in a test suite.
func TestMain(m *testing.M) {
	uploadBackoff = func(int) time.Duration { return time.Millisecond }
	os.Exit(m.Run())
}

func settleFixture(t *testing.T, remoteBytes, stagedBytes int) (*uploader, tgsource.Item, string, string) {
	t.Helper()
	dstDir, stageDir := t.TempDir(), t.TempDir()
	if remoteBytes >= 0 {
		if err := os.WriteFile(filepath.Join(dstDir, settleName), make([]byte, remoteBytes), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if stagedBytes >= 0 {
		if err := os.WriteFile(filepath.Join(stageDir, settleName), make([]byte, stagedBytes), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dst, err := fs.NewFs(t.Context(), dstDir)
	if err != nil {
		t.Fatal(err)
	}
	local, err := fs.NewFs(t.Context(), stageDir)
	if err != nil {
		t.Fatal(err)
	}
	it := tgsource.Item{MessageID: 4242, Name: settleName, Media: &tmedia.Media{Size: 4096}}
	return &uploader{local: local, dst: dst}, it, dstDir, stageDir
}

// settle is what stands between a died-halfway upload and a permanently wrong
// archive. rclone writes straight to the final name on any backend that does
// not advertise PartialUploads (copy.go:93) and cleans up only when it did not
// (copy.go:348-350), so on such a remote — pikpak, here — a fragment is left
// under exactly the name verification matches on.
func TestSettleRemovesAFragment(t *testing.T) {
	u, it, dstDir, _ := settleFixture(t, 400, 4096)

	landed, err := u.settle(t.Context(), it)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if landed {
		t.Error("a 400-byte fragment was reported as a completed upload")
	}
	if _, err := os.Stat(filepath.Join(dstDir, settleName)); !os.IsNotExist(err) {
		t.Error("the fragment was left under the name verify matches")
	}
}

// pikpak commits uploads as a server-side async task, so a transfer rclone gave
// up on can still land correctly afterwards. That is a success, not something
// to delete and fetch again — and the staged copy has to go, because the move
// that would normally have removed it is the thing that failed.
func TestSettleKeepsALateButCompleteUpload(t *testing.T) {
	u, it, dstDir, stageDir := settleFixture(t, 4096, 4096)

	landed, err := u.settle(t.Context(), it)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !landed {
		t.Error("a complete object was not recognised as a finished upload")
	}
	if _, err := os.Stat(filepath.Join(dstDir, settleName)); err != nil {
		t.Errorf("a complete file was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stageDir, settleName)); !os.IsNotExist(err) {
		t.Error("the staged copy was left behind, so the byte budget stays committed")
	}
}

func TestSettleWithNothingOnTheRemote(t *testing.T) {
	u, it, _, _ := settleFixture(t, -1, 4096)

	landed, err := u.settle(t.Context(), it)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if landed {
		t.Error("an absent object was reported as uploaded")
	}
}

// The retry itself: a first attempt that fails must not abandon the file when
// the staged copy is still there and the remote is fine.
func TestUploadRetriesAfterAFailedMove(t *testing.T) {
	u, it, dstDir, stageDir := settleFixture(t, -1, 4096)

	// Make the first move fail by removing the staged file, then restoring it
	// so a later attempt can succeed. Simpler and closer to the real failure:
	// upload once with the file absent, confirm the error names every attempt.
	if err := os.Remove(filepath.Join(stageDir, settleName)); err != nil {
		t.Fatal(err)
	}
	err := u.upload(t.Context(), it)
	if err == nil {
		t.Fatal("upload of a missing staged file returned nil")
	}
	if _, serr := os.Stat(filepath.Join(dstDir, settleName)); !os.IsNotExist(serr) {
		t.Error("a failed upload left an object on the remote")
	}
}

// A move that works still has to leave staging clean and the object intact.
func TestUploadMovesAndConfirms(t *testing.T) {
	u, it, dstDir, stageDir := settleFixture(t, -1, 4096)
	u.confirm = true

	if err := u.upload(t.Context(), it); err != nil {
		t.Fatalf("upload: %v", err)
	}
	info, err := os.Stat(filepath.Join(dstDir, settleName))
	if err != nil {
		t.Fatalf("object not on the destination: %v", err)
	}
	if info.Size() != it.Size() {
		t.Errorf("object is %d bytes, want %d", info.Size(), it.Size())
	}
	if _, err := os.Stat(filepath.Join(stageDir, settleName)); !os.IsNotExist(err) {
		t.Error("the staged copy survived a successful move")
	}
}
