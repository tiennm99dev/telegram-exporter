package report

import (
	"strings"
	"testing"
	"time"
)

func TestHumanCountGroupsThousands(t *testing.T) {
	cases := map[int]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000",
		12000: "12,000", 11406: "11,406", 1234567: "1,234,567",
	}
	for in, want := range cases {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", in, got, want)
		}
	}
}

// A redirected run must not accumulate ANSI redraws: that is what turned the
// shell pipeline's captured logs into megabytes of control characters.
func TestTickerWritesNoAnsiWhenRedirected(t *testing.T) {
	var sb strings.Builder
	tick := NewTicker(&sb, "messages read")

	for i := 1; i <= 5000; i++ {
		tick.Update(i)
	}
	tick.Done(5000)

	out := sb.String()
	if strings.ContainsAny(out, "\r\033") {
		t.Errorf("redirected output contains control characters: %q", out)
	}
	if !strings.Contains(out, "5,000 messages read") {
		t.Errorf("final count missing from %q", out)
	}
}

// Update is called once per message on an 18k-message walk, so it has to be
// cheap: at most one line per interval, however often it is called.
func TestTickerThrottlesUpdates(t *testing.T) {
	var sb strings.Builder
	tick := NewTicker(&sb, "objects listed")
	tick.lastLine = time.Now() // inside the interval from the start

	for i := 1; i <= 10000; i++ {
		tick.Update(i)
	}
	if n := strings.Count(sb.String(), "\n"); n != 0 {
		t.Errorf("wrote %d lines inside one interval, want 0", n)
	}

	tick.Done(10000)
	if !strings.Contains(sb.String(), "10,000 objects listed in") {
		t.Errorf("Done did not state the total: %q", sb.String())
	}
}
