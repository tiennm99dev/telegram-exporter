package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// spaceCheckInterval is how often the destination's free space is re-read
// mid-run. About is a network round trip, so it is not worth doing per file.
const spaceCheckInterval = time.Minute

// spaceGuard watches the destination's free space during a run.
//
// Checking only before starting is not enough on a long archive: an 18k-message
// chat runs for hours, and a remote that was fine at the start can fill in the
// middle — from this run's own uploads, or from anything else using the account.
// Without this the run discovers it by failing several multi-gigabyte uploads in
// a row, which costs the download bandwidth for all of them.
//
// A backend that cannot report a quota is treated as unlimited, matching the
// pre-flight check and the shell pipeline before it.
type spaceGuard struct {
	free    func(context.Context) (int64, bool)
	minFree int64

	mu        sync.Mutex
	lastCheck time.Time
	failed    error
}

func newSpaceGuard(free func(context.Context) (int64, bool), minFree int64) *spaceGuard {
	if free == nil || minFree <= 0 {
		return nil
	}
	return &spaceGuard{free: free, minFree: minFree, lastCheck: time.Now()}
}

// check reports an error once the destination has dropped below the floor.
//
// The verdict is sticky: once the remote is known to be full, every later call
// says so without another round trip, because the run is ending either way.
func (g *spaceGuard) check(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.failed != nil {
		return g.failed
	}
	if time.Since(g.lastCheck) < spaceCheckInterval {
		return nil
	}
	g.lastCheck = time.Now()

	free, ok := g.free(ctx)
	if !ok || free >= g.minFree {
		return nil
	}
	g.failed = fmt.Errorf("%w: only %.1f GiB free, below the %.1f GiB floor",
		ErrDestinationFailing, float64(free)/(1<<30), float64(g.minFree)/(1<<30))
	return g.failed
}
