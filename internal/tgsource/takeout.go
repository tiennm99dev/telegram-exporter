package tgsource

import (
	"context"
	"sync"

	"github.com/gotd/td/tg"

	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/middlewares/takeout"
)

// safeTakeout wraps a pool to make its takeout path survive a failed init.
//
// core's own dcpool.Takeout deadlocks on that path. It holds the pool's mutex
// for the whole call, and its recovery from a failed init is to return
// p.Client(ctx, dc) — which locks the same mutex again (dcpool.go:113-121 and
// :57-58). sync.Mutex is not reentrant, so the worker blocks forever, then every
// other worker blocks behind it, and the process hangs with no output and no
// response to cancellation, since the goroutine is parked on a mutex rather than
// a select.
//
// That is not an exotic path. Telegram answers account.initTakeoutSession with
// TAKEOUT_INIT_DELAY when a takeout was started recently — tdl's own "ignore
// init delay error" comment shows it expects exactly this — and takeout is on by
// default, so running two exports in succession is enough to trigger it.
//
// Probing before the run is not an alternative: a probe would consume an init
// and make the pool's own init the one that gets the delay error. So the takeout
// session is established here instead, once, and the pool's Takeout is never
// called at all.
type safeTakeout struct {
	dcpool.Pool

	once sync.Once
	id   int64
	ok   bool
}

func withSafeTakeout(p dcpool.Pool) dcpool.Pool { return &safeTakeout{Pool: p} }

// Takeout returns a takeout-scoped client, or an ordinary one if no takeout
// session could be established.
//
// Falling back rather than failing matches what core intended: takeout raises
// rate limits and reaches older history, but a download works without it. The
// difference is that this fallback returns.
func (s *safeTakeout) Takeout(ctx context.Context, dc int) *tg.Client {
	base := s.Pool.Client(ctx, dc)

	s.once.Do(func() {
		id, err := takeout.Takeout(ctx, base.Invoker())
		if err != nil {
			return // ok stays false; every caller gets a plain client
		}
		s.id, s.ok = id, true
	})
	if !s.ok {
		return base
	}
	return tg.NewClient(takeout.Middleware(s.id).Handle(base.Invoker()))
}
