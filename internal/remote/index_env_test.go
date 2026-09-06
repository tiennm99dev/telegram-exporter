package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
)

// rclone reads RCLONE_* into its global config at package init, so an in-process
// t.Setenv is too late to prove anything. A subprocess is the only way to
// observe what BuildIndex actually does under an operator's environment — which
// is exactly why the original no-op neutralisation went unnoticed.
func TestBuildIndexIgnoresInheritedFilters(t *testing.T) {
	if os.Getenv("GO_INDEX_ENV_CHILD") == "1" {
		indexChild(t)
		return
	}
	for _, env := range []string{
		"RCLONE_EXCLUDE=*.mp4",
		"RCLONE_FILTER=- *.mp4",
		"RCLONE_MIN_SIZE=1M",
		"RCLONE_MAX_AGE=1h",
		"RCLONE_MAX_DEPTH=1",
	} {
		t.Run(env, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestBuildIndexIgnoresInheritedFilters", "-test.v")
			cmd.Env = append(os.Environ(), "GO_INDEX_ENV_CHILD=1", env)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("%s narrowed the index:\n%s", env, out)
			}
		})
	}
}

func indexChild(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"a.mp4", "b.txt", "sub/c.mp4"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := fs.NewFs(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := BuildIndex(t.Context(), f, 1234567890, nil)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	for _, want := range []string{"a.mp4", "b.txt", "c.mp4"} {
		if _, ok := idx.Lookup(want); !ok {
			t.Errorf("%q missing from the index", want)
		}
	}
}
