package report

import (
	"fmt"
	"io"

	"github.com/tiennm99dev/telegram-exporter/internal/verify"
)

// writeSurvey states what the chat holds and how much of it is already archived,
// broken out by why each outstanding file is outstanding. The single "N to
// fetch" it replaces hid the difference between never-fetched, empty,
// wrong-size, and unarchivable — which is the difference between a run that will
// converge and one that cannot.
func Survey(w io.Writer, r verify.Report) {
	fmt.Fprintf(w, "\n  chat holds     %s media, %s\n",
		humanCount(r.Expected), humanBytes(r.Bytes))
	fmt.Fprintf(w, "  archived       %s\n", humanCount(r.Present))

	for _, row := range []struct {
		label string
		n     int
	}{
		{"never fetched", len(r.Absent) - len(r.Unsafe)},
		{"zero-byte", len(r.ZeroByte)},
		{"wrong size", len(r.Mismatched)},
		{"unarchivable", len(r.Unsafe)},
	} {
		if row.n > 0 {
			fmt.Fprintf(w, "    %-13s %s\n", row.label, humanCount(row.n))
		}
	}
}

// PlanInfo is what the run is about to do, stated before it starts.
type PlanInfo struct {
	Files              int
	Bytes              int64
	Largest            int64
	Budget             int64
	Staging            string
	Threads            int
	Downloads, Uploads int
	Destination        string
}

// writePlan prints the settings that decide how long the run takes and how much
// disk it uses, so an operator can stop it before a multi-hour transfer rather
// than discover the wrong cap partway through.
func Plan(w io.Writer, p PlanInfo) {
	cap := "uncapped"
	if p.Budget > 0 {
		cap = "capped at " + humanBytes(p.Budget)
	}
	fmt.Fprintf(w, "\n  fetching       %s files, %s (largest %s)\n",
		humanCount(p.Files), humanBytes(p.Bytes), humanBytes(p.Largest))
	fmt.Fprintf(w, "  into           %s\n", p.Destination)
	fmt.Fprintf(w, "  staging        %s, %s\n", p.Staging, cap)
	fmt.Fprintf(w, "  concurrency    %d download(s) x %d thread(s), %d upload(s)\n\n",
		p.Downloads, p.Threads, p.Uploads)
}
