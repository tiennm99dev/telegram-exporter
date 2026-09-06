package report

import (
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/rclone/rclone/fs"
	rclonelog "github.com/rclone/rclone/fs/log"
)

// CaptureRcloneLog routes rclone's own log lines to w.
//
// rclone writes to stderr through its private logger on its own schedule
// (fs/log/slog.go:43 installs a stderr handler via fs.SetLogger). While bars are
// drawing, that lands mid-redraw and shreds the display — a run hitting a series
// of pikpak commit failures became unreadable. Pointing the logger at the
// progress writer makes each line scroll above the bars instead.
//
// The returned function puts the logger back on stderr. rclone exposes no getter
// for the current handler, so this restores the default rather than whatever was
// there before; nothing in this program installs a third one.
func CaptureRcloneLog(ctx context.Context, w io.Writer) (restore func()) {
	level := fs.LogLevelToSlog(fs.GetConfig(ctx).LogLevel)
	fs.SetLogger(rclonelog.NewOutputHandler(w, &slog.HandlerOptions{Level: level}, 0))
	return func() {
		fs.SetLogger(rclonelog.NewOutputHandler(os.Stderr, &slog.HandlerOptions{Level: level}, 0))
	}
}
