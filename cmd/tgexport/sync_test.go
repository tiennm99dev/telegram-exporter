package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
	"github.com/tiennm99dev/telegram-exporter/internal/verify"
)

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"":     0, // unset means no cap
		"40G":  40 << 30,
		"40g":  40 << 30,
		"512M": 512 << 20,
		"2T":   2 << 40,
		"1024": 1024, // bare number is bytes
		"1K":   1 << 10,
	}
	for in, want := range ok {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q) = %v, want %d", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}

	for _, in := range []string{"G", "0", "0G", "-5G", "40GB", "4.5G", "abc", "40Q"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", in, got)
		}
	}
}

func syncItem(id int, size int64) tgsource.Item {
	m := &tmedia.Media{Name: "f.mp4", Size: size}
	return tgsource.Item{DialogID: 1, MessageID: id, Name: naming.For(1, id, m), Media: m}
}

func TestSelectTodoPicksOnlyOutstandingItems(t *testing.T) {
	items := []tgsource.Item{syncItem(1, 10), syncItem(2, 10), syncItem(3, 10), syncItem(4, 10)}
	r := verify.Report{Absent: []int{2, 4}, ZeroByte: []int{3}}

	got := selectTodo(items, r, 0)
	if len(got) != 3 {
		t.Fatalf("selected %d items, want 3", len(got))
	}
	for _, it := range got {
		if it.MessageID == 1 {
			t.Error("selected an item that is already archived")
		}
	}
}

// An unsafe name is kept in the report so it stays visible, but queuing it for
// download would retry something that can never succeed — the non-terminating
// loop this design exists to avoid.
func TestSelectTodoSkipsUnsafeNames(t *testing.T) {
	items := []tgsource.Item{syncItem(1, 10), syncItem(2, 10)}
	r := verify.Report{
		Absent: []int{1, 2},
		Unsafe: []verify.Unsafe{{MessageID: 2, Name: "../x", Reason: errors.New("unsafe")}},
	}

	got := selectTodo(items, r, 0)
	if len(got) != 1 || got[0].MessageID != 1 {
		t.Errorf("selected %+v, want only message 1", got)
	}
}

func TestSelectTodoHonoursLimit(t *testing.T) {
	items := []tgsource.Item{syncItem(1, 10), syncItem(2, 10), syncItem(3, 10)}
	r := verify.Report{Absent: []int{1, 2, 3}}

	if got := selectTodo(items, r, 2); len(got) != 2 {
		t.Errorf("selected %d items with a limit of 2, want 2", len(got))
	}
	if got := selectTodo(items, r, 0); len(got) != 3 {
		t.Errorf("selected %d items with no limit, want 3", len(got))
	}
}

// A cap below the largest file could never admit it, so the run would block on
// something it can never start — which from outside looks like a stalled remote.
// It has to be refused up front.
func TestValidateBudgetRejectsCapBelowLargestFile(t *testing.T) {
	todo := []tgsource.Item{syncItem(1, 1<<20), syncItem(2, 5<<30)}

	err := validateBudget(4<<30, todo)
	if err == nil {
		t.Fatal("a 4 GiB cap was accepted with a 5 GiB file to fetch")
	}
	if !errors.Is(err, errUsage) {
		t.Errorf("error should be a usage error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "largest file") {
		t.Errorf("error should name the problem, got: %v", err)
	}

	if err := validateBudget(6<<30, todo); err != nil {
		t.Errorf("a 6 GiB cap should accept a 5 GiB file, got: %v", err)
	}
	if err := validateBudget(0, todo); err != nil {
		t.Errorf("an unset cap should accept anything, got: %v", err)
	}
}
