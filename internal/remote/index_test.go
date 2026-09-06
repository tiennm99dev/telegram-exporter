package remote

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
)

const testDialog = int64(1234567890)

// localIndex builds an index over a temp directory using rclone's local
// backend, so BuildIndex is exercised through the same listing path a real
// remote uses rather than through a stub.
func localIndex(t *testing.T, files map[string]int) *Index {
	t.Helper()
	dir := t.TempDir()
	for name, size := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %q: %v", name, err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o600); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}

	ctx := context.Background()
	f, err := fs.NewFs(ctx, dir)
	if err != nil {
		t.Fatalf("open local fs: %v", err)
	}
	idx, err := BuildIndex(ctx, f, testDialog, nil)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	return idx
}

func TestLookupMatchesWholeNamesOnly(t *testing.T) {
	idx := localIndex(t, map[string]int{
		"1234567890_4242_Pipe her! And by her, we mean pipeperr! 1080p.mp4": 4096,
		"1234567890_14726_298.mp4": 2048,
	})

	if got, ok := idx.Lookup("1234567890_14726_298.mp4"); !ok || got != 2048 {
		t.Errorf("Lookup(exact) = %d, %v; want 2048, true", got, ok)
	}

	// The doubled '!' is the name Telegram reports; the remote holds the
	// collapsed one that tdl's filenamify template wrote. Those are different
	// files as far as this index is concerned, and that is the whole policy.
	wanted := "1234567890_4242_Pipe her!! And by her, we mean pipeperr! 1080p.mp4"
	if _, ok := idx.Lookup(wanted); ok {
		t.Error("Lookup matched a near-miss name; presence must require an exact match")
	}
}

// A remote written with a subdirectory layout has to match a flat one, because
// existing archives were written both ways.
func TestBuildIndexReducesPathsToBasename(t *testing.T) {
	idx := localIndex(t, map[string]int{
		"nested/dir/1234567890_42_deep.mp4": 512,
	})
	if got, ok := idx.Lookup("1234567890_42_deep.mp4"); !ok || got != 512 {
		t.Errorf("Lookup after basename reduction = %d, %v; want 512, true", got, ok)
	}
}

func TestStoredUnderOtherNamesFindsStaleCopies(t *testing.T) {
	stale := "1234567890_4242_Pipe her! And by her, we mean pipeperr! 1080p.mp4"
	idx := localIndex(t, map[string]int{stale: 4096})

	wanted := "1234567890_4242_Pipe her!! And by her, we mean pipeperr! 1080p.mp4"
	others := idx.StoredUnderOtherNames(4242, wanted)
	if len(others) != 1 || others[0] != stale {
		t.Fatalf("StoredUnderOtherNames = %v, want [%q]", others, stale)
	}

	// The wanted name itself is never reported as an "other" name.
	idx2 := localIndex(t, map[string]int{wanted: 4096})
	if others := idx2.StoredUnderOtherNames(4242, wanted); len(others) != 0 {
		t.Errorf("StoredUnderOtherNames = %v, want empty when the wanted name is present", others)
	}
}

// Objects belonging to a different dialog, or not matching the stored layout at
// all, must not be indexed by id — otherwise an unrelated file could be reported
// as a stale copy of a message.
func TestStoredUnderOtherNamesIgnoresForeignObjects(t *testing.T) {
	idx := localIndex(t, map[string]int{
		"999999_4242_other-dialog.mp4": 100,
		"not-a-tdl-name.mp4":           100,
	})
	if others := idx.StoredUnderOtherNames(4242, "1234567890_4242_x.mp4"); len(others) != 0 {
		t.Errorf("StoredUnderOtherNames = %v, want empty", others)
	}
}

func TestIndexLenCountsEveryObject(t *testing.T) {
	idx := localIndex(t, map[string]int{
		"1234567890_1_a.mp4": 1,
		"1234567890_2_b.mp4": 1,
		"unrelated.txt":      1,
	})
	if idx.Len() != 3 {
		t.Errorf("Len() = %d, want 3", idx.Len())
	}
}

func TestBuildIndexOnEmptyRemote(t *testing.T) {
	idx := localIndex(t, nil)
	if idx.Len() != 0 {
		t.Errorf("Len() = %d, want 0", idx.Len())
	}
	if _, ok := idx.Lookup("anything"); ok {
		t.Error("Lookup on an empty index reported a hit")
	}
}

// Two objects in different directories can share a basename. Listing order is
// not guaranteed, so silently keeping one would make the verdict flip between
// runs; the ambiguity has to be reported instead.
func TestBuildIndexReportsBasenameCollisions(t *testing.T) {
	idx := localIndex(t, map[string]int{
		"a/1234567890_42_same.mp4": 100,
		"b/1234567890_42_same.mp4": 0,
	})

	dup := idx.Collisions()
	if len(dup) != 1 || dup[0] != "1234567890_42_same.mp4" {
		t.Fatalf("Collisions() = %v, want the shared basename reported", dup)
	}
	if idx.Len() != 1 {
		t.Errorf("Len() = %d, want 1 — a dropped collision must not be counted", idx.Len())
	}

	// The id map must not gain a duplicate entry either, or a delete would try
	// the same name twice and fail the second time.
	others := idx.StoredUnderOtherNames(42, "1234567890_42_wanted.mp4")
	if len(others) != 1 {
		t.Errorf("StoredUnderOtherNames = %v, want one entry, not a duplicate", others)
	}
}

// Acting on an object needs the path rclone knows it by, which differs from the
// basename used for matching whenever the remote has directory structure.
// Deleting by basename would miss the object, or hit the wrong one.
func TestPathOfReturnsTheFullRemotePath(t *testing.T) {
	idx := localIndex(t, map[string]int{"nested/dir/1234567890_42_deep.mp4": 512})

	const name = "1234567890_42_deep.mp4"
	if _, ok := idx.Lookup(name); !ok {
		t.Fatalf("Lookup(%q) missed; matching is by basename", name)
	}

	got, ok := idx.PathOf(name)
	if !ok {
		t.Fatalf("PathOf(%q) reported no match", name)
	}
	if want := "nested/dir/" + name; got != want {
		t.Errorf("PathOf(%q) = %q, want %q — deleting by basename would target the wrong path", name, got, want)
	}
}

func TestPathOfMissesUnknownNames(t *testing.T) {
	idx := localIndex(t, nil)
	if p, ok := idx.PathOf("absent.mp4"); ok {
		t.Errorf("PathOf(absent) = %q, true; want no match", p)
	}
}
