package pipeline

import (
	"context"
	"errors"
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
// Both failure paths end at dropShort, and that is the part that matters. An
// object of the wrong size sitting under the right name is worse than no object
// at all: verification matches on name and non-zero size, so it would be counted
// archived by this run and by every run after it — permanently, once the local
// copy is gone. Removing it turns a silent corruption into an absent file the
// next run fetches again.
func (u *uploader) upload(ctx context.Context, it tgsource.Item) error {
	if err := operations.MoveFile(ctx, u.dst, u.local, it.Name, it.Name); err != nil {
		err = fmt.Errorf("move %q to %s: %w", it.Name, u.dst.String(), err)
		// A failed move can still leave a partial object under the final name.
		// rclone only writes to a temporary name when the backend advertises
		// PartialUploads (copy.go:93), and it only cleans up after itself when
		// it did (copy.go:348-350) — pikpak, the remote this was built against,
		// advertises neither, so a died-halfway transfer stays exactly where a
		// complete one would be.
		return errors.Join(err, u.dropShort(ctx, it))
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
		if derr := remove(ctx, obj); derr != nil {
			return fmt.Errorf("%w (and it could not be removed: %v — delete it by hand "+
				"or verify will count it archived)", err, derr)
		}
		return err
	}
	return nil
}

// dropShort removes an object left under it.Name at the wrong size.
//
// An object of the *right* size is deliberately left alone. pikpak commits an
// upload as a server-side async task, so a transfer rclone gave up on can still
// land correctly afterwards; deleting it on the strength of the error alone
// would throw away a good file and force it to be fetched again.
func (u *uploader) dropShort(ctx context.Context, it tgsource.Item) error {
	// A fresh context: the run may already be shutting down, which is one of
	// the ways the move failed in the first place.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	obj, err := u.dst.NewObject(ctx, it.Name)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		return nil // nothing was left behind
	}
	if err != nil {
		return fmt.Errorf("check for a leftover %q: %w", it.Name, err)
	}
	if obj.Size() == it.Size() {
		return nil
	}
	if derr := remove(ctx, obj); derr != nil {
		return fmt.Errorf("a %d-byte fragment of %q (expected %d) is on the remote and "+
			"could not be removed: %w — delete it by hand or verify will count it archived",
			obj.Size(), it.Name, it.Size(), derr)
	}
	return nil
}

// remove deletes an object on a context that outlives the run's cancellation.
func remove(ctx context.Context, obj fs.Object) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return operations.DeleteFile(ctx, obj)
}
