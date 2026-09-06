// Package tgsource owns the Telegram side: building an authenticated client from
// a tdl session and pooling connections across data centres.
package tgsource

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/telegram"

	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/core/storage/keygen"
	"github.com/iyear/tdl/core/tclient"
)

// defaultReconnectTimeout matches tdl's own default.
const defaultReconnectTimeout = 5 * time.Minute

// app describes the Telegram application a session was authorised against.
//
// A session is bound to the app that created it, so the AppID/AppHash used here
// must match the ones `tdl login` used or Telegram rejects the auth key. tdl
// records the choice in the store under the "app" key and defaults to its own
// application when the key is absent; mirror that exactly rather than hardcoding
// one, or a session created with `tdl login -d` fails to open.
type app struct {
	id   int
	hash string
}

var apps = map[string]app{
	// Application registered by tdl's author; tdl's default.
	"builtin": {id: 15055931, hash: "021d433426cbb920eeb95164498fe3d3"},
	// Telegram Desktop's application, used by `tdl login -d`.
	"desktop": {id: 2040, hash: "b18441a1ff607e10a989891a5462e627"},
}

// Options configures a session built from an existing tdl login.
type Options struct {
	KV               storage.Storage
	Proxy            string
	NTP              string
	ReconnectTimeout time.Duration
	PoolSize         int64
}

// Session is an authenticated Telegram client together with the settings needed
// to build a DC pool over it.
//
// A gotd client cannot be reused: telegram.Client.Run refuses a second call once
// the first has returned. Anything that needs to retry a whole run must build a
// new Session rather than calling Run twice.
type Session struct {
	client   *telegram.Client
	timeout  time.Duration
	poolSize int64
}

// New builds a Telegram session from the credentials in kv.
//
// Nothing connects yet: gotd dials inside Run, so callers must do their work in
// the callback Run provides.
func New(ctx context.Context, o Options) (*Session, error) {
	a, err := resolveApp(ctx, o.KV)
	if err != nil {
		return nil, err
	}

	if o.ReconnectTimeout == 0 {
		o.ReconnectTimeout = defaultReconnectTimeout
	}
	if o.PoolSize == 0 {
		o.PoolSize = 8 // tdl's default DC pool size
	}

	// Middlewares are deliberately left empty here. core/tclient.New already
	// prepends NewDefaultMiddlewares (recovery, retry, floodwait) to whatever it
	// is given, so passing them again nests retry inside retry — three levels
	// deep once core's own copy is counted, turning a hard RPC failure into
	// minutes of silent backoff. The pool gets them explicitly in Run instead;
	// see the comment there.
	client, err := tclient.New(ctx, tclient.Options{
		AppID:            a.id,
		AppHash:          a.hash,
		Session:          storage.NewSession(o.KV, false),
		Proxy:            o.Proxy,
		NTP:              o.NTP,
		ReconnectTimeout: o.ReconnectTimeout,
	})
	if err != nil {
		return nil, err
	}

	return &Session{client: client, timeout: o.ReconnectTimeout, poolSize: o.PoolSize}, nil
}

// Client exposes the underlying client for calls that do not need the pool.
// Only valid inside a Run callback.
func (s *Session) Client() *telegram.Client { return s.client }

func resolveApp(ctx context.Context, kv storage.Storage) (app, error) {
	mode := "builtin"
	if v, err := kv.Get(ctx, keygen.New("app")); err == nil {
		mode = string(v)
	}
	a, ok := apps[mode]
	if !ok {
		return app{}, fmt.Errorf("session records unknown app %q; re-run `tdl login`", mode)
	}
	return a, nil
}

// Run connects, verifies the session is authorised, and invokes fn with a DC
// pool. The pool is closed before Run returns.
func (s *Session) Run(ctx context.Context, fn func(context.Context, dcpool.Pool) error) error {
	err := s.client.Run(ctx, func(ctx context.Context) error {
		status, err := s.client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		if !status.Authorized {
			return fmt.Errorf("tdl session is not authorized; run `tdl login`")
		}

		// gotd applies a client's middlewares only to Client.Invoke. Calls made
		// through a pooled DC connection bypass them entirely, so the pool needs
		// its own copy or downloads run with no flood-wait handling and no retry
		// — which on a multi-hour archive dies at the first FLOOD_WAIT. tdl does
		// the same thing for the same reason (app/dl/dl.go).
		pool := dcpool.NewPool(s.client, s.poolSize,
			tclient.NewDefaultMiddlewares(ctx, s.timeout)...)
		defer func() { _ = pool.Close() }()

		return fn(ctx, pool)
	})

	// gotd swallows cancellation: telegram.Client.Run ends with
	//   if err := g.Wait(); !errors.Is(err, context.Canceled) { return err }
	//   return nil
	// so an interrupted run — and any callback error wrapping context.Canceled —
	// comes back as success. Reporting that as exit 0 would tell a driver the
	// archive is complete when it was abandoned half-way, which is exactly the
	// failure the shell pipeline's `trap ... exit 130` existed to prevent.
	if err == nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
	}
	return err
}
