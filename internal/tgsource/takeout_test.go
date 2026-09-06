package tgsource

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/iyear/tdl/core/dcpool"
)

// failingInvoker refuses every call, which is what Telegram does to
// account.initTakeoutSession with TAKEOUT_INIT_DELAY.
type failingInvoker struct{ calls atomic.Int64 }

func (f *failingInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	f.calls.Add(1)
	return errors.New("TAKEOUT_INIT_DELAY_86400")
}

type fakePool struct{ inv tg.Invoker }

func (p *fakePool) Client(context.Context, int) *tg.Client { return tg.NewClient(p.inv) }
func (p *fakePool) Takeout(context.Context, int) *tg.Client {
	panic("upstream Takeout must not be called")
}
func (p *fakePool) Default(context.Context) *tg.Client { return tg.NewClient(p.inv) }
func (p *fakePool) Close() error                       { return nil }

// core's dcpool.Takeout holds the pool mutex and recovers from a failed init by
// calling Client, which locks the same mutex — so the upstream version of this
// test hangs instead of failing. The wrapper must return a usable client.
func TestSafeTakeoutFallsBackWhenInitFails(t *testing.T) {
	inv := &failingInvoker{}
	var pool dcpool.Pool = withSafeTakeout(&fakePool{inv: inv})

	done := make(chan *tg.Client, 1)
	go func() { done <- pool.Takeout(t.Context(), 2) }()

	select {
	case got := <-done:
		if got == nil {
			t.Fatal("Takeout returned nil after a failed init")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Takeout deadlocked after a failed init")
	}

	// The init is attempted once, not once per file: a failing init that ran on
	// every element would add a round trip to each of 18k downloads.
	before := inv.calls.Load()
	for range 5 {
		if pool.Takeout(t.Context(), 2) == nil {
			t.Fatal("Takeout returned nil")
		}
	}
	if got := inv.calls.Load(); got != before {
		t.Errorf("takeout init retried %d times after failing; want no retries", got-before)
	}
}
