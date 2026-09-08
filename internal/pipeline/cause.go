package pipeline

import (
	"context"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/iyear/tdl/core/logctx"
)

// captureCauses points core's downloader logging back into this package.
//
// core's Download never returns why a transfer failed. It logs the error and
// returns nil (downloader.go:47-60), so the size check in finish is the only
// evidence anything went wrong — and a size check can say how many bytes
// arrived, never why the rest did not. Nothing here installed a logger either,
// which means logctx handed out zap.NewNop() and the reason was destroyed: a
// failed two-gigabyte fetch was reported as "short download: got 0 bytes" with
// the flood wait, expired file reference, or dead connection behind it gone.
//
// So the error is taken from the one place it exists. The entry carries the
// element it belongs to, which is this package's own *elem, so the error is
// stored on it and finish reports it beside the size.
func captureCauses(ctx context.Context) context.Context {
	return logctx.With(ctx, zap.New(&causeCore{}))
}

// causeCore is a zapcore.Core that keeps error entries and drops everything
// else. Fields are matched on their type rather than their key so a rename
// upstream cannot silently switch the capture off.
type causeCore struct{ fields []zapcore.Field }

func (c *causeCore) Enabled(l zapcore.Level) bool { return l >= zapcore.ErrorLevel }

func (c *causeCore) With(fields []zapcore.Field) zapcore.Core {
	joined := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	joined = append(joined, c.fields...)
	joined = append(joined, fields...)
	return &causeCore{fields: joined}
}

func (c *causeCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}

// Write pairs a logged error with the element it was logged for.
//
// The write happens on the download worker's own goroutine, immediately before
// the deferred OnDone that reads it, so storing the error on the element needs
// no synchronisation.
func (c *causeCore) Write(_ zapcore.Entry, fields []zapcore.Field) error {
	var (
		el  *elem
		err error
	)
	for _, set := range [][]zapcore.Field{c.fields, fields} {
		for _, f := range set {
			switch f.Type {
			case zapcore.ErrorType:
				if e, ok := f.Interface.(error); ok {
					err = e
				}
			case zapcore.ReflectType:
				if e, ok := f.Interface.(*elem); ok {
					el = e
				}
			}
		}
	}
	if el != nil && err != nil {
		el.cause = err
	}
	return nil
}

func (c *causeCore) Sync() error { return nil }
