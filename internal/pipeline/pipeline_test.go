package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iyear/tdl/core/tmedia"

	"github.com/tiennm99dev/telegram-exporter/internal/naming"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// The budget is what replaces run.sh's du-polling and SIGSTOP/SIGCONT cap
// draining, so the property it has to hold is simple and worth pinning: the sum
// of outstanding reservations never exceeds the limit.
func TestBudgetBoundsOutstandingBytes(t *testing.T) {
	const limit = 1000
	b := newBudget(limit)
	ctx := t.Context()

	var (
		mu       sync.Mutex
		held     int64
		peak     int64
		wg       sync.WaitGroup
		acquires atomic.Int64
	)

	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			const size = 300
			if err := b.acquire(ctx, size); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			acquires.Add(1)

			mu.Lock()
			held += size
			if held > peak {
				peak = held
			}
			mu.Unlock()

			time.Sleep(time.Millisecond)

			mu.Lock()
			held -= size
			mu.Unlock()
			b.release(size)
		}()
	}
	wg.Wait()

	if acquires.Load() != 20 {
		t.Errorf("acquired %d times, want 20 — every item must eventually get through", acquires.Load())
	}
	if peak > limit {
		t.Errorf("peak outstanding = %d bytes, over the %d limit", peak, limit)
	}
}

// A zero limit means the operator asked for no cap; acquiring must not block or
// account, or an unbounded run would stall.
func TestBudgetUnboundedWhenLimitIsZero(t *testing.T) {
	b := newBudget(0)
	for range 5 {
		if err := b.acquire(t.Context(), 1<<40); err != nil {
			t.Fatalf("acquire on an unbounded budget: %v", err)
		}
	}
	b.release(1 << 40) // must not panic
}

// Cancelling must unblock a waiter rather than leaving the run wedged.
func TestBudgetAcquireHonoursCancellation(t *testing.T) {
	b := newBudget(100)
	if err := b.acquire(t.Context(), 100); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- b.acquire(ctx, 100) }()

	// The second acquire cannot succeed while the first is outstanding.
	select {
	case err := <-done:
		t.Fatalf("acquire succeeded with no space free: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("acquire error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling did not unblock the waiter")
	}
}

// An item bigger than the whole budget can never be admitted. It must fail
// rather than hang, because a hang here looks exactly like a slow remote.
func TestBudgetRefusesAnItemLargerThanTheLimit(t *testing.T) {
	b := newBudget(100)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	err := b.acquire(ctx, 5000)
	if err == nil {
		t.Fatal("acquire of an oversized item succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("acquire error = %v, want the deadline to surface", err)
	}
}

func TestResultFailedListsOnlyErrors(t *testing.T) {
	r := Result{Outcomes: []Outcome{
		{Item: mustItem(1), Err: nil},
		{Item: mustItem(2), Err: errors.New("flood wait")},
		{Item: mustItem(3), Err: nil},
		{Item: mustItem(4), Err: errors.New("short download")},
	}}
	failed := r.Failed()
	if len(failed) != 2 {
		t.Fatalf("Failed() = %d entries, want 2", len(failed))
	}
	for _, f := range failed {
		if f.Err == nil {
			t.Errorf("Failed() returned a successful outcome: %+v", f)
		}
	}
}

func mustItem(id int) tgsource.Item {
	m := &tmedia.Media{Name: "f.mp4", Size: 10}
	return tgsource.Item{DialogID: 1, MessageID: id, Name: naming.For(1, id, m), Media: m}
}
