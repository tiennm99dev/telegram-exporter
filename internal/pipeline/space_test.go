package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSpaceGuard(t *testing.T) {
	const floor = 10 << 30

	t.Run("unset guard never complains", func(t *testing.T) {
		if err := newSpaceGuard(nil, floor).check(t.Context()); err != nil {
			t.Errorf("check = %v, want nil when no reporter is configured", err)
		}
	})

	t.Run("a backend without a quota counts as unlimited", func(t *testing.T) {
		g := newSpaceGuard(func(context.Context) (int64, bool) { return 0, false }, floor)
		g.lastCheck = time.Now().Add(-2 * spaceCheckInterval)
		if err := g.check(t.Context()); err != nil {
			t.Errorf("check = %v, want nil when the backend cannot report", err)
		}
	})

	t.Run("plenty of room is fine", func(t *testing.T) {
		g := newSpaceGuard(func(context.Context) (int64, bool) { return 100 << 30, true }, floor)
		g.lastCheck = time.Now().Add(-2 * spaceCheckInterval)
		if err := g.check(t.Context()); err != nil {
			t.Errorf("check = %v, want nil with 100 GiB free", err)
		}
	})

	t.Run("below the floor stops the run", func(t *testing.T) {
		var calls int
		g := newSpaceGuard(func(context.Context) (int64, bool) {
			calls++
			return 1 << 30, true
		}, floor)
		g.lastCheck = time.Now().Add(-2 * spaceCheckInterval)

		err := g.check(t.Context())
		if !errors.Is(err, ErrDestinationFailing) {
			t.Fatalf("check = %v, want ErrDestinationFailing", err)
		}
		// Sticky, and without another round trip: the run is ending either way,
		// and every upload worker calls this.
		for range 5 {
			if !errors.Is(g.check(t.Context()), ErrDestinationFailing) {
				t.Fatal("the verdict did not stick")
			}
		}
		if calls != 1 {
			t.Errorf("free space was read %d times, want 1", calls)
		}
	})

	t.Run("checks are throttled", func(t *testing.T) {
		var calls int
		g := newSpaceGuard(func(context.Context) (int64, bool) {
			calls++
			return 100 << 30, true
		}, floor)
		// Freshly built, so the interval has not elapsed. About is a network
		// round trip and every worker calls this per file.
		for range 20 {
			if err := g.check(t.Context()); err != nil {
				t.Fatalf("check = %v", err)
			}
		}
		if calls != 0 {
			t.Errorf("free space was read %d times inside the interval, want 0", calls)
		}
	})
}
