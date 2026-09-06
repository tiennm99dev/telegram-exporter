package pipeline

import (
	"context"
	"fmt"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"

	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// uploader moves finished files from staging to the destination remote.
type uploader struct {
	local   fs.Fs // the staging directory as an rclone filesystem
	dst     fs.Fs
	confirm bool
}

// upload moves one finished file to the remote and, unless disabled, proves it
// arrived at the expected size.
//
// MoveFile removes the local copy as part of the move, so a successful return
// means the file is on the remote and off local disk — which is what lets the
// byte budget be released.
//
// Confirmation closes a gap the shell pipeline left open: there, a truncated
// upload was only noticed by a later verify pass, after the local copy was
// already gone. Re-stating the object costs one round trip per file and turns a
// silent corruption into a retry.
func (u *uploader) upload(ctx context.Context, it tgsource.Item) error {
	if err := operations.MoveFile(ctx, u.dst, u.local, it.Name, it.Name); err != nil {
		return fmt.Errorf("move %q to %s: %w", it.Name, u.dst.String(), err)
	}
	if !u.confirm {
		return nil
	}

	obj, err := u.dst.NewObject(ctx, it.Name)
	if err != nil {
		return fmt.Errorf("confirm %q: %w", it.Name, err)
	}
	if got := obj.Size(); got != it.Size() {
		return fmt.Errorf("confirm %q: remote has %d bytes, expected %d", it.Name, got, it.Size())
	}
	return nil
}
