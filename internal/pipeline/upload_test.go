package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// dropShort is what stands between a died-halfway upload and a permanently
// wrong archive. rclone writes straight to the final name on any backend that
// does not advertise PartialUploads (copy.go:93) and cleans up only when it did
// not (copy.go:348-350), so on such a remote — pikpak, here — a fragment is left
// under exactly the name verification matches on.
func TestDropShortRemovesAFragmentButKeepsACompleteFile(t *testing.T) {
	const name = "-100123_4242_clip.mp4"
	const want = 4096

	cases := map[string]struct {
		staged    int // bytes already at the destination; -1 means absent
		wantThere bool
	}{
		// A fragment must go: left alone, verify counts it archived by name and
		// non-zero size, for this run and every run after it.
		"fragment is removed": {staged: 400, wantThere: false},
		// A complete file must stay. pikpak commits uploads as a server-side
		// async task, so a transfer rclone gave up on can still land correctly;
		// deleting on the strength of the error alone throws away a good file.
		"complete file is kept": {staged: want, wantThere: true},
		"nothing to clean up":   {staged: -1, wantThere: false},
	}

	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			dstDir := t.TempDir()
			path := filepath.Join(dstDir, name)
			if tc.staged >= 0 {
				if err := os.WriteFile(path, make([]byte, tc.staged), 0o600); err != nil {
					t.Fatalf("stage destination file: %v", err)
				}
			}
			dst, err := fs.NewFs(t.Context(), dstDir)
			if err != nil {
				t.Fatalf("open destination: %v", err)
			}

			u := &uploader{dst: dst}
			it := tgsource.Item{MessageID: 4242, Name: name, Media: &tmedia.Media{Size: want}}
			if err := u.dropShort(t.Context(), it); err != nil {
				t.Fatalf("dropShort: %v", err)
			}

			_, serr := os.Stat(path)
			switch {
			case tc.wantThere && serr != nil:
				t.Errorf("a complete file was deleted: %v", serr)
			case !tc.wantThere && serr == nil:
				t.Error("a short object was left under the name verify matches")
			}
		})
	}
}
