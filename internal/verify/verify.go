// Package verify answers whether a chat is fully archived on a remote.
//
// It replaces verify-export.sh and keeps that script's size judgements, which
// were arrived at by watching real failures rather than by taste.
//
// One judgement is deliberately not carried over. The script also counted files
// sitting in the staging directory as present, because download and upload were
// separate processes and a file could be finished locally but not yet uploaded
// for a whole sync interval. Here a single process owns both legs, so that state
// is not one a verify can meaningfully observe — except after an interrupted
// run, where staging may hold completed files. Phase 5 owns staging and decides
// whether to credit it; until then a verify after an interrupt may report files
// absent that are on local disk, and re-fetch them.
package verify

import (
	"fmt"
	"io"
	"sort"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// Index is the part of a remote snapshot verification needs.
//
// An interface rather than *remote.Index so the two questions stay separable:
// presence is answered from a whole name, and the id-keyed lookup is explicitly
// a different method with a name that says it is not an answer. It also lets the
// report be tested without a remote.
type Index interface {
	// Lookup reports the size stored under an exact name.
	Lookup(name string) (size int64, ok bool)
	// StoredUnderOtherNames lists names present for a message id that are not
	// the wanted name. Diagnostics only; never a presence answer.
	StoredUnderOtherNames(messageID int, wanted string) []string
}

// tinyThreshold is the size below which a present file is reported for a human
// to look at but still trusted. Some real media genuinely is this small, so
// treating it as damaged would re-download it forever.
const tinyThreshold = 1024

// Misnamed is a wanted file that is absent, while some other file is stored
// under the same message id.
type Misnamed struct {
	MessageID int
	Wanted    string
	Found     []string
}

// Unsafe is a wanted file whose name cannot be written to a path.
type Unsafe struct {
	MessageID int
	Name      string
	Reason    error
}

// Tiny is a present file small enough to be worth a look.
type Tiny struct {
	MessageID int
	Name      string
	Size      int64
}

// Report is the outcome of comparing a chat against a remote.
type Report struct {
	Expected int   // media messages in the chat
	Present  int   // present, non-empty
	Bytes    int64 // total size of everything expected

	Absent   []int // not on the remote under the wanted name
	ZeroByte []int // present but empty

	Misnamed []Misnamed
	Unsafe   []Unsafe
	Tiny     []Tiny
}

// Todo lists the message ids needing another fetch, in ascending order.
//
// Zero-byte files are included: rclone overwrites a size-mismatched destination,
// so simply fetching again repairs them.
func (r Report) Todo() []int {
	todo := make([]int, 0, len(r.Absent)+len(r.ZeroByte))
	todo = append(todo, r.Absent...)
	todo = append(todo, r.ZeroByte...)
	sort.Ints(todo)
	return todo
}

// Complete reports whether every expected file is present and non-empty.
func (r Report) Complete() bool { return len(r.Todo()) == 0 }

// Check compares the wanted items against an index of the remote.
//
// Matching is on the whole name. A file stored under any other name is not the
// file that was asked for, however close it looks — that is a deliberate policy,
// and the near-misses are collected into Misnamed rather than being quietly
// accepted, because the re-download lands beside them and both copies stay.
func Check(items []tgsource.Item, idx Index) Report {
	r := Report{Expected: len(items)}

	for _, it := range items {
		r.Bytes += it.Size()

		if err := naming.Safe(it.Name); err != nil {
			// Never counted present: this name cannot be written anywhere safe,
			// so no correctly-behaving run could have archived it.
			r.Unsafe = append(r.Unsafe, Unsafe{MessageID: it.MessageID, Name: it.Name, Reason: err})
			r.Absent = append(r.Absent, it.MessageID)
			continue
		}

		size, ok := idx.Lookup(it.Name)
		switch {
		case !ok:
			r.Absent = append(r.Absent, it.MessageID)
			if others := idx.StoredUnderOtherNames(it.MessageID, it.Name); len(others) > 0 {
				r.Misnamed = append(r.Misnamed, Misnamed{
					MessageID: it.MessageID, Wanted: it.Name, Found: others,
				})
			}
		case size == 0:
			r.ZeroByte = append(r.ZeroByte, it.MessageID)
		default:
			r.Present++
			if size < tinyThreshold {
				r.Tiny = append(r.Tiny, Tiny{MessageID: it.MessageID, Name: it.Name, Size: size})
			}
		}
	}
	return r
}

// Write renders a report in the shape verify-export.sh printed, so the numbers
// stay comparable across the cutover.
func (r Report) Write(w io.Writer) {
	fmt.Fprintf(w, "media expected     : %d  (%.1f GiB)\n", r.Expected, float64(r.Bytes)/(1<<30))
	fmt.Fprintf(w, "present and intact : %d\n", r.Present)
	fmt.Fprintf(w, "  absent           : %d\n", len(r.Absent))
	fmt.Fprintf(w, "  zero-byte        : %d\n", len(r.ZeroByte))

	if len(r.Tiny) > 0 {
		fmt.Fprintf(w, "  under 1KiB (check, not retried): %d\n", len(r.Tiny))
		for _, t := range r.Tiny[:min(5, len(r.Tiny))] {
			fmt.Fprintf(w, "      id %d  %d B  %q\n", t.MessageID, t.Size, t.Name)
		}
	}

	if len(r.Unsafe) > 0 {
		fmt.Fprintf(w, "\nunsafe filenames   : %d\n", len(r.Unsafe))
		fmt.Fprintf(w, "  these cannot be written to a path and are never fetched:\n")
		for _, u := range r.Unsafe {
			fmt.Fprintf(w, "      id %d  %v\n", u.MessageID, u.Reason)
		}
	}

	if len(r.Misnamed) > 0 {
		fmt.Fprintf(w, "\nstored under a different name : %d\n", len(r.Misnamed))
		fmt.Fprintf(w, "  counted as absent and fetched again; delete the stale copies so the\n")
		fmt.Fprintf(w, "  re-download does not leave two files for the same message:\n")
		for _, m := range r.Misnamed {
			fmt.Fprintf(w, "      id %d\n        wanted: %q\n", m.MessageID, m.Wanted)
			for _, f := range m.Found {
				fmt.Fprintf(w, "        remote: %q\n", f)
			}
		}
	}

	if todo := r.Todo(); len(todo) > 0 {
		fmt.Fprintf(w, "\nneeds another pass : %d  (ids %d–%d)\n", len(todo), todo[0], todo[len(todo)-1])
		return
	}
	fmt.Fprintf(w, "\nCOMPLETE: every media message is present and non-empty.\n")
}
