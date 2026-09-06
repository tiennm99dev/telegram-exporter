package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
)

// The exit-code contract is what a driver script reads to decide whether the
// archive is finished. Every mapping is asserted here because the previous
// version of this code documented the contract in a comment and then returned
// nil on interruption, which a driver would have read as "complete".
func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		sig  os.Signal
		want int
	}{
		{"success", nil, nil, exitOK},
		{"help is not a failure", flag.ErrHelp, nil, exitOK},
		{"wrapped help", fmt.Errorf("parse: %w", flag.ErrHelp), nil, exitOK},
		{"usage mistake", fmt.Errorf("%w: bad flag", errUsage), nil, exitUsage},
		{"run left work outstanding", fmt.Errorf("%w: 12 files", errIncomplete), nil, exitIncomplete},
		{"remote failure", errors.New("pikpak unreachable"), nil, exitRemoteError},
		{"cancelled without a signal", context.Canceled, nil, exitSIGINT},
		{"wrapped cancellation", fmt.Errorf("download: %w", context.Canceled), nil, exitSIGINT},
		{"SIGINT", nil, os.Interrupt, exitSIGINT},
		{"SIGTERM", nil, syscall.SIGTERM, exitSIGTERM},

		// A signal outranks the error it produced. Without this, an interrupted
		// run whose cleanup happened to fail would exit 3 and look like a remote
		// problem instead of an operator stop.
		{"signal outranks error", errors.New("upload aborted"), os.Interrupt, exitSIGINT},
		{"signal outranks success", nil, syscall.SIGTERM, exitSIGTERM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err, tt.sig); got != tt.want {
				t.Errorf("exitCode(%v, %v) = %d, want %d", tt.err, tt.sig, got, tt.want)
			}
		})
	}
}

// Codes must not collide: a driver distinguishes them by value alone.
func TestExitCodesMatchShellPipeline(t *testing.T) {
	// run.sh: 0 ok, 2 usage, 3 rclone failure, 130 SIGINT, 143 SIGTERM.
	// export-until-complete.sh: 1 ran but still incomplete.
	want := map[string]int{
		"ok": 0, "incomplete": 1, "usage": 2, "remote": 3, "sigint": 130, "sigterm": 143,
	}
	got := map[string]int{
		"ok": exitOK, "incomplete": exitIncomplete, "usage": exitUsage,
		"remote": exitRemoteError, "sigint": exitSIGINT, "sigterm": exitSIGTERM,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s exit code = %d, want %d (shell pipeline contract)", k, got[k], w)
		}
	}
}

func TestNotifyContextReportsSignal(t *testing.T) {
	ctx, signalled := notifyContext()

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled within 5s of SIGINT")
	}

	if sig := signalled(); sig != syscall.SIGINT {
		t.Errorf("signalled() = %v, want SIGINT", sig)
	}
}

// An uninterrupted command must not be reported as signalled, or every clean
// run would exit 130.
func TestNotifyContextReportsNoSignal(t *testing.T) {
	ctx, signalled := notifyContext()

	if sig := signalled(); sig != nil {
		t.Errorf("signalled() = %v on a clean run, want nil", sig)
	}
	// The accessor also releases the watcher, which cancels the context.
	if ctx.Err() == nil {
		t.Error("context should be cancelled once the watcher is released")
	}
}
