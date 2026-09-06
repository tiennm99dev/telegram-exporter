// Command tgexport archives Telegram chat media to an rclone remote.
//
// It replaces a three-script shell pipeline that ran `tdl dl` and `rclone move`
// as separate processes. Both are embedded here as libraries, so the program can
// see a download finish rather than inferring it from a filename suffix and a
// file's age.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	// Registers the rclone storage backends this binary can talk to. Backend
	// selection is a property of the binary, so the import lives here rather
	// than in a library package where it would leak into every importer and
	// make the `slim` build tag meaningless.
	_ "github.com/tiennm99dev/telegram-exporter/internal/backends"
)

// Exit codes, matching the shell pipeline so existing habits and any wrapper
// scripts keep working: run.sh used 0 ok, 2 usage, 3 rclone failure, 130 SIGINT,
// 143 SIGTERM, and export-until-complete.sh used 1 for "ran, still incomplete".
const (
	exitOK          = 0
	exitIncomplete  = 1
	exitUsage       = 2
	exitRemoteError = 3
	exitSIGINT      = 130
	exitSIGTERM     = 143
)

// errUsage marks an error as the operator's mistake rather than a failure,
// selecting exit code 2.
var errUsage = errors.New("usage")

// errIncomplete marks a run that finished cleanly but left work outstanding.
var errIncomplete = errors.New("incomplete")

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		usage()
		return exitUsage
	}
	if a := os.Args[1]; a == "-h" || a == "--help" || a == "help" {
		usage()
		return exitOK
	}

	ctx, signalled := notifyContext()

	var err error
	switch os.Args[1] {
	case "doctor":
		err = doctorCmd(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		return exitUsage
	}

	sig := signalled()
	if sig != nil {
		fmt.Fprintf(os.Stderr, "interrupted (%v)\n", sig)
	} else if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	return exitCode(err, sig)
}

// exitCode maps a command's outcome onto the shell pipeline's contract.
//
// A signal outranks whatever error the interruption produced on the way out:
// the operator stopped this, and the code has to say so rather than letting a
// driver read an abandoned run as finished.
func exitCode(err error, sig os.Signal) int {
	if sig != nil {
		if sig == syscall.SIGTERM {
			return exitSIGTERM
		}
		return exitSIGINT
	}

	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.Is(err, context.Canceled):
		return exitSIGINT
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.Is(err, errIncomplete):
		return exitIncomplete
	default:
		return exitRemoteError
	}
}

// notifyContext cancels ctx on SIGINT or SIGTERM and reports which arrived.
//
// Unlike signal.NotifyContext it stops trapping after the first signal, so a
// second Ctrl-C kills the process outright. That matters when shutdown itself
// hangs — an rclone upload waiting on a slow pikpak commit, say — and the
// operator needs a way out that does not involve another terminal.
func notifyContext() (context.Context, func() os.Signal) {
	ctx, cancel := context.WithCancel(context.Background())

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	var got os.Signal
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		select {
		case sig := <-ch:
			got = sig
			signal.Stop(ch) // next one gets the default disposition: die
			cancel()
		case <-done:
			signal.Stop(ch)
			cancel()
		}
	}()

	// Waiting on finished before reading got is what makes the read safe: the
	// goroutine writes it and then closes the channel, so the happens-before
	// edge is the close, not the return.
	return ctx, func() os.Signal {
		close(done)
		<-finished
		return got
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage: tgexport <command> [options]

Commands:
  doctor    Check the Telegram session, the destination remote, and free space

Run 'tgexport <command> -h' for command options.
`)
}
