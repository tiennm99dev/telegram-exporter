package verify

import (
	"slices"
	"strings"
	"testing"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

const dialog = int64(1234567890)

// fakeIndex is a remote snapshot expressed directly, so report logic is tested
// without a network or a filesystem.
type fakeIndex map[string]int64

func (f fakeIndex) Lookup(name string) (int64, bool) {
	size, ok := f[name]
	return size, ok
}

func (f fakeIndex) StoredUnderOtherNames(messageID int, wanted string) []string {
	var others []string
	for name := range f {
		if name == wanted {
			continue
		}
		if id, ok := naming.SplitStored(dialog, name); ok && id == messageID {
			others = append(others, name)
		}
	}
	slices.Sort(others)
	return others
}

func item(msgID int, file string, size int64) tgsource.Item {
	m := &tmedia.Media{Name: file, Size: size}
	return tgsource.Item{
		DialogID:  dialog,
		MessageID: msgID,
		Name:      naming.For(dialog, msgID, m),
		Media:     m,
	}
}

func TestCheckClassifiesEveryOutcome(t *testing.T) {
	items := []tgsource.Item{
		item(1, "present.mp4", 5000),
		item(2, "empty.mp4", 5000),
		item(3, "absent.mp4", 5000),
		item(4, "tiny.jpg", 500),
	}
	idx := fakeIndex{
		"1234567890_1_present.mp4": 5000,
		"1234567890_2_empty.mp4":   0,
		"1234567890_4_tiny.jpg":    500,
	}

	r := Check(items, idx)

	if r.Expected != 4 {
		t.Errorf("Expected = %d, want 4", r.Expected)
	}
	if r.Present != 2 {
		t.Errorf("Present = %d, want 2 (the non-empty ones)", r.Present)
	}
	if !slices.Equal(r.Absent, []int{3}) {
		t.Errorf("Absent = %v, want [3]", r.Absent)
	}
	if !slices.Equal(r.ZeroByte, []int{2}) {
		t.Errorf("ZeroByte = %v, want [2]", r.ZeroByte)
	}

	// Sub-1KiB is reported but still counted present: some real media is
	// genuinely that small, so retrying it would loop forever.
	if len(r.Tiny) != 1 || r.Tiny[0].MessageID != 4 {
		t.Errorf("Tiny = %v, want just message 4", r.Tiny)
	}
	if slices.Contains(r.Todo(), 4) {
		t.Error("a tiny file must not be queued for another fetch")
	}

	// Zero-byte files are retried: rclone overwrites a size-mismatched
	// destination, so fetching again repairs them.
	if want := []int{2, 3}; !slices.Equal(r.Todo(), want) {
		t.Errorf("Todo() = %v, want %v", r.Todo(), want)
	}
	if r.Complete() {
		t.Error("Complete() = true with work outstanding")
	}
}

func TestCheckCompleteWhenEverythingIsPresent(t *testing.T) {
	items := []tgsource.Item{item(1, "a.mp4", 10), item(2, "b.mp4", 20)}
	idx := fakeIndex{"1234567890_1_a.mp4": 10, "1234567890_2_b.mp4": 20}

	r := Check(items, idx)
	if !r.Complete() {
		t.Fatalf("Complete() = false, Todo() = %v", r.Todo())
	}
	var sb strings.Builder
	r.Write(&sb)
	if !strings.Contains(sb.String(), "COMPLETE") {
		t.Errorf("report should say COMPLETE, got:\n%s", sb.String())
	}
}

// The message that motivated the rewrite: the wanted name has a doubled '!',
// the remote holds the collapsed one that tdl's template wrote. It must be
// absent, and the stale copy must be surfaced rather than silently accepted.
func TestCheckReportsMisnamedCopiesAsAbsent(t *testing.T) {
	items := []tgsource.Item{item(4242, "Pipe her!! And by her, we mean pipeperr! 1080p.mp4", 966444937)}
	stale := "1234567890_4242_Pipe her! And by her, we mean pipeperr! 1080p.mp4"
	idx := fakeIndex{stale: 966444937}

	r := Check(items, idx)

	if !slices.Equal(r.Absent, []int{4242}) {
		t.Errorf("Absent = %v, want [4242] — a near-miss name is a different file", r.Absent)
	}
	if r.Present != 0 {
		t.Errorf("Present = %d, want 0", r.Present)
	}
	if len(r.Misnamed) != 1 || !slices.Equal(r.Misnamed[0].Found, []string{stale}) {
		t.Fatalf("Misnamed = %+v, want the stale copy reported", r.Misnamed)
	}

	var sb strings.Builder
	r.Write(&sb)
	out := sb.String()
	for _, want := range []string{"stored under a different name", stale, "delete the stale copies"} {
		if !strings.Contains(out, want) {
			t.Errorf("report should mention %q, got:\n%s", want, out)
		}
	}
}

// A name that cannot be written to a path is never counted present and never
// silently skipped — it is reported and queued, so it stays visible.
func TestCheckFlagsUnsafeNames(t *testing.T) {
	items := []tgsource.Item{item(7, "../../../.config/rclone/rclone.conf", 100)}

	r := Check(items, fakeIndex{})

	if len(r.Unsafe) != 1 || r.Unsafe[0].MessageID != 7 {
		t.Fatalf("Unsafe = %+v, want message 7 flagged", r.Unsafe)
	}
	if !slices.Equal(r.Absent, []int{7}) {
		t.Errorf("Absent = %v, want [7]", r.Absent)
	}
	if r.Present != 0 {
		t.Errorf("Present = %d, want 0", r.Present)
	}

	var sb strings.Builder
	r.Write(&sb)
	if !strings.Contains(sb.String(), "unsafe filenames") {
		t.Errorf("report should flag the unsafe name, got:\n%s", sb.String())
	}
}

// An unsafe name must be rejected even when something is stored under that
// message id: the index is not the authority on whether a name is writable.
func TestCheckUnsafeNameIsNeverPresent(t *testing.T) {
	items := []tgsource.Item{item(7, "sub/dir.mp4", 100)}
	idx := fakeIndex{"1234567890_7_sub/dir.mp4": 100}

	if r := Check(items, idx); r.Present != 0 || len(r.Unsafe) != 1 {
		t.Errorf("Present = %d, Unsafe = %+v; an unsafe name must never count present", r.Present, r.Unsafe)
	}
}

func TestReportTodoIsSorted(t *testing.T) {
	r := Report{Absent: []int{4242, 3}, ZeroByte: []int{100}}
	if want := []int{3, 100, 4242}; !slices.Equal(r.Todo(), want) {
		t.Errorf("Todo() = %v, want %v", r.Todo(), want)
	}
}
