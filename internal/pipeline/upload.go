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

// uploadAttempts is how many times a file is offered to the remote before the
// run gives up on it.
//
// Retrying here rather than fetching again next pass is the whole point: when a
// move fails the local copy is still in staging, so another attempt costs a few
// seconds, while abandoning it costs re-downloading the file from Telegram —
// which for this archive can be two gigabytes.
const uploadAttempts = 3

// uploader moves finished files from staging to the destination remote.
type uploader struct {
	local   fs.Fs // the staging directory as an rclone filesystem
	dst     fs.Fs
	confirm bool
}

// upload moves one finished file to the remote, retrying a failure.
//
// pikpak is why this retries at all. It commits an upload as a server-side async
// task, and rclone polls that task only as long as its low-level retries last
// (backend/pikpak/helper.go:205-214, bounded by fs.NewPacer). A task still in
// PHASE_TYPE_PENDING when the polling budget runs out is reported as
// "can't verify the task is completed", and observed against the live archive
// that task then never commits — the object is simply absent afterwards. The
// transfer itself was fine; only the confirmation timed out.
func (u *uploader) upload(ctx context.Context, it tgsource.Item) error {
	var errs []error
	for attempt := 1; ; attempt++ {
		// Every attempt after the first looks before it leaps. A pending task
		// from the previous attempt may have committed during the backoff, and
		// pikpak allows two files with the same name — so re-uploading without
		// checking is how one file becomes two, which verification then reports
		// as an ambiguous basename forever.
		if attempt > 1 {
			landed, err := u.settle(ctx, it)
			if err != nil {
				errs = append(errs, err)
			}
			if landed {
				return nil
			}
		}

		err := u.attempt(ctx, it)
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("attempt %d/%d: %w", attempt, uploadAttempts, err))

		if attempt >= uploadAttempts || ctx.Err() != nil {
			return errors.Join(errs...)
		}
		if err := sleep(ctx, uploadBackoff(attempt)); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
}

// attempt runs one move and, unless disabled, proves the object arrived at the
// expected size.
//
// MoveFile removes the local copy as part of the move, so a successful return
// means the file is on the remote and off local disk.
func (u *uploader) attempt(ctx context.Context, it tgsource.Item) error {
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
		if derr := remove(ctx, obj); derr != nil {
			return fmt.Errorf("%w (and it could not be removed: %v — delete it by hand "+
				"or verify will count it archived)", err, derr)
		}
		return err
	}
	return nil
}

// settle reports whether the file is on the remote at the right size, cleaning
// up if it is there at the wrong one.
//
// A failed move can leave a partial object under the final name. rclone only
// writes to a temporary name when the backend advertises PartialUploads
// (copy.go:93) and only cleans up after itself when it did (copy.go:348-350) —
// pikpak advertises neither, so a died-halfway transfer stays exactly where a
// complete one would be, and verification would count it archived forever.
//
// An object of the right size means the upload actually succeeded, however the
// move reported itself, so the staged copy is removed here: nothing downstream
// will do it on a success path that MoveFile did not take.
func (u *uploader) settle(ctx context.Context, it tgsource.Item) (landed bool, err error) {
	// A fresh context: the run may already be shutting down, which is one of
	// the ways the move failed in the first place.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	obj, err := u.dst.NewObject(ctx, it.Name)
	if errors.Is(err, fs.ErrorObjectNotFound) {
		return false, nil // nothing was left behind
	}
	if err != nil {
		return false, fmt.Errorf("check for a leftover %q: %w", it.Name, err)
	}

	if obj.Size() == it.Size() {
		if src, serr := u.local.NewObject(ctx, it.Name); serr == nil {
			if derr := operations.DeleteFile(ctx, src); derr != nil {
				return true, fmt.Errorf("%q reached the remote but the staged copy "+
					"could not be removed: %w", it.Name, derr)
			}
		}
		return true, nil
	}

	if derr := remove(ctx, obj); derr != nil {
		return false, fmt.Errorf("a %d-byte fragment of %q (expected %d) is on the remote and "+
			"could not be removed: %w — delete it by hand or verify will count it archived",
			obj.Size(), it.Name, it.Size(), derr)
	}
	return false, nil
}

// uploadBackoff spaces out retries. pikpak's commit queue is what is being
// waited on, and it is measured in seconds rather than milliseconds.
//
// A var so tests can shorten it: at the real cadence a single test that proves
// a retry happens spends half a minute asleep.
var uploadBackoff = func(attempt int) time.Duration {
	return time.Duration(attempt) * 10 * time.Second
}

// sleep waits, or returns early if the run is cancelled.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// remove deletes an object on a context that outlives the run's cancellation.
func remove(ctx context.Context, obj fs.Object) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return operations.DeleteFile(ctx, obj)
}
