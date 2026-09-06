package report

import (
	"strings"
	"testing"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/pipeline"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
	"github.com/tiennm99dev/telegram-exporter/internal/verify"
)

func TestSurveyBreaksOutdWhyFilesAreOutstanding(t *testing.T) {
	r := verify.Report{Expected: 12000, Present: 11400, Bytes: 518 << 30}
	r.Absent = append(r.Absent, 1, 2, 3)
	r.ZeroByte = append(r.ZeroByte, 4)
	r.Mismatched = append(r.Mismatched, verify.Mismatch{MessageID: 5})
	r.Unsafe = append(r.Unsafe, verify.Unsafe{MessageID: 3})

	var sb strings.Builder
	Survey(&sb, r)
	out := sb.String()

	for _, want := range []string{"12,000 media", "11,400", "zero-byte", "wrong size", "unarchivable"} {
		if !strings.Contains(out, want) {
			t.Errorf("survey missing %q:\n%s", want, out)
		}
	}
	// Unsafe ids are inside Absent, so counting both would double-count them and
	// the rows would not add up to the outstanding total.
	if !strings.Contains(out, "never fetched 2") {
		t.Errorf("never-fetched should exclude the unarchivable id:\n%s", out)
	}
}

func TestSurveyOmitsEmptyRows(t *testing.T) {
	var sb strings.Builder
	Survey(&sb, verify.Report{Expected: 10, Present: 10})
	if strings.Contains(sb.String(), "zero-byte") {
		t.Errorf("a clean archive should list no failure rows:\n%s", sb.String())
	}
}

func TestPlanStatesTheCap(t *testing.T) {
	var sb strings.Builder
	Plan(&sb, PlanInfo{Files: 2613, Bytes: 79 << 30, Largest: 2 << 30,
		Staging: "./staging", Threads: 4, Downloads: 2, Uploads: 2, Destination: "remote:x"})
	if !strings.Contains(sb.String(), "uncapped") {
		t.Errorf("a run with no budget must say so:\n%s", sb.String())
	}

	sb.Reset()
	Plan(&sb, PlanInfo{Files: 1, Budget: 40 << 30, Staging: "./staging", Destination: "remote:x"})
	if !strings.Contains(sb.String(), "40.0 GiB") {
		t.Errorf("plan should state the cap:\n%s", sb.String())
	}
}

// The redirected path must stay free of cursor movement: bars in a captured log
// are megabytes of control characters, which is what tdl's progress bar did to
// the shell pipeline's logs.
func TestReporterWritesNoAnsi(t *testing.T) {
	var sb strings.Builder
	r := newReporter(&sb, 3, 300)
	it := tgsource.Item{MessageID: 1, Name: "a.mp4", Media: &tmedia.Media{Size: 100}}

	r.DownloadStart(it)
	for i := range 500 {
		r.DownloadBytes(it, int64(i))
		r.Stats(pipeline.Stats{Done: 1, BytesDone: int64(i)})
	}
	r.DownloadDone(it, nil)
	r.UploadStart(it)
	r.UploadDone(it, nil)
	r.Finish(pipeline.Stats{Done: 1, BytesDone: 100})

	out := sb.String()
	if strings.ContainsAny(out, "\r\033") {
		t.Errorf("redirected output contains control characters: %q", out)
	}
	// A completed file is still recorded, so a captured run says what moved.
	if !strings.Contains(out, "archived") || !strings.Contains(out, "a.mp4") {
		t.Errorf("redirected output should record each archived file:\n%s", out)
	}
}

// Events picks the renderer from the writer: a strings.Builder is not a
// terminal, so it must never get bars.
func TestEventsChoosesTheLineRendererOffTerminal(t *testing.T) {
	var sb strings.Builder
	if _, ok := Events(&sb, 1, 1).(*Reporter); !ok {
		t.Error("a non-terminal writer got the bar renderer")
	}
}
