package naming

import (
	"path/filepath"
	"strings"
	"testing"
)

// Names come from DocumentAttributeFilename, which whoever uploaded the file
// chose. Anything that is not a single path element must be refused before it
// reaches filepath.Join.
func TestSafeRejectsNamesThatEscapeADirectory(t *testing.T) {
	bad := []struct {
		name string
		want string
	}{
		{"", "empty"},
		{".", "directory reference"},
		{"..", "directory reference"},
		{"../escape.mp4", "single path element"},
		{"../../../.config/rclone/rclone.conf", "single path element"},
		{"sub/dir.mp4", "single path element"},
		{"trailing/", "single path element"},
		{"/etc/passwd", "absolute path"},
		{"/", "absolute path"},
		{"nul\x00byte.mp4", "NUL byte"},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			err := Safe(tt.name)
			if err == nil {
				t.Fatalf("Safe(%q) = nil; this name escapes or breaks a path", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Safe(%q) error = %v, want it to mention %q", tt.name, err, tt.want)
			}
		})
	}
}

// Ordinary media names, including awkward but legal ones, must pass — a
// containment check that rejects real files is just a different outage.
func TestSafeAcceptsRealNames(t *testing.T) {
	for _, name := range []string{
		"1234567890_4242_Pipe her!! And by her, we mean pipeperr! 1080p.mp4",
		"1234567890_14726_298.mp4",
		"1234567890_8_ünïcödé näme 🍓.mp4",
		"1234567890_42_a?b:c.mp4",  // reserved on Windows, fine here
		"1234567890_7_...dots.mp4", // leading dots inside the field, not the name
		"1234567890_9_-dash.mp4",
		"1234567890_10_ leading-space.mp4",
	} {
		if err := Safe(name); err != nil {
			t.Errorf("Safe(%q) = %v, want nil", name, err)
		}
	}
}

// The property that actually matters: a name Safe accepts cannot, once joined,
// resolve outside the directory it was joined to.
func TestSafeNamesStayInsideTheStagingDirectory(t *testing.T) {
	const staging = "/var/tmp/staging"
	for _, name := range []string{
		"1234567890_1_ok.mp4",
		"1234567890_2_..dots.mp4",
		"1234567890_3_a..b.mp4",
	} {
		if err := Safe(name); err != nil {
			t.Fatalf("Safe(%q) = %v, want nil", name, err)
		}
		joined := filepath.Clean(filepath.Join(staging, name))
		if filepath.Dir(joined) != staging {
			t.Errorf("Join(%q, %q) = %q, which leaves the staging directory", staging, name, joined)
		}
	}
}

func TestSplitStoredRoundTripsWhatForProduces(t *testing.T) {
	const dialog = int64(1234567890)
	for _, msgID := range []int{1, 42, 4242, 4246} {
		name := For(dialog, msgID, mediaNamed("a_b!!.mp4"))
		got, ok := SplitStored(dialog, name)
		if !ok {
			t.Errorf("SplitStored(%q) reported no match", name)
			continue
		}
		if got != msgID {
			t.Errorf("SplitStored(%q) = %d, want %d", name, got, msgID)
		}
	}
}

func TestSplitStoredRejectsForeignNames(t *testing.T) {
	const dialog = int64(1234567890)
	for _, name := range []string{
		"999_42_other-dialog.mp4",    // different dialog
		"1234567890_notanumber_.mp4", // id is not a number
		"1234567890_0_zero.mp4",      // ids start at 1
		"1234567890_042_pad.mp4",     // For never emits leading zeros
		"1234567890_+42_sign.mp4",    // nor a sign
		"1234567890_-5_neg.mp4",
		"1234567890_42", // no filename field
		"1234567890",    // no id field
		"",
	} {
		if id, ok := SplitStored(dialog, name); ok {
			t.Errorf("SplitStored(%q) = %d, true; want no match", name, id)
		}
	}
}

// A dialog id that is a prefix of another must not match it.
func TestSplitStoredDoesNotMatchPrefixOverlap(t *testing.T) {
	if id, ok := SplitStored(123456789, "1234567890_42_file.mp4"); ok {
		t.Errorf("SplitStored matched a longer dialog id, got %d", id)
	}
}

// An over-long name is the one input that can hang a drive-until-complete loop:
// os.Create rejects it, so the download fails forever while verify keeps
// reporting it absent. It must be refused up front, not discovered per pass.
func TestSafeRejectsNamesOverTheFilesystemLimit(t *testing.T) {
	prefix := "1234567890_42_"
	fill := 255 - len(prefix)

	atLimit := prefix + strings.Repeat("a", fill)
	if err := Safe(atLimit); err != nil {
		t.Errorf("Safe(%d bytes) = %v, want nil at exactly the limit", len(atLimit), err)
	}

	overLimit := prefix + strings.Repeat("a", fill+1)
	err := Safe(overLimit)
	if err == nil {
		t.Fatalf("Safe(%d bytes) = nil, want an error past the limit", len(overLimit))
	}
	if !strings.Contains(err.Error(), "over the 255-byte limit") {
		t.Errorf("error should name the limit, got: %v", err)
	}

	// The limit is bytes, not runes: multi-byte names hit it sooner.
	multibyte := prefix + strings.Repeat("é", 130) // 260 bytes of payload
	if err := Safe(multibyte); err == nil {
		t.Errorf("Safe(%d bytes, %d runes) = nil; the limit must count bytes",
			len(multibyte), len([]rune(multibyte)))
	}
}
