package pipeline

import (
	"context"
	"fmt"
	"time"

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
// means the file is on the remote and off local disk.
//
// A short object is deleted rather than left in place, and that is the part that
// matters. Verification matches on name and non-zero size, so a truncated object
// under the right name would be counted archived by this run and by every run
// after it — permanently, with the local copy already gone. Removing it turns a
// silent corruption into an absent file the next run fetches again. The shell
// pipeline had this hole too: it noticed a bad upload only at the next verify,
// by which point the evidence was the same.
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
		err := fmt.Errorf("confirm %q: remote has %d bytes, expected %d", it.Name, got, it.Size())
		// Deleted on a fresh context: the run may already be shutting down, and
		// leaving a plausible-looking short object behind is worse than the
		// error that got us here.
		delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if derr := operations.DeleteFile(delCtx, obj); derr != nil {
			return fmt.Errorf("%w (and it could not be removed: %v — delete it by hand "+
				"or verify will count it archived)", err, derr)
		}
		return err
	}
	return nil
}
