package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/iyear/tdl/core/dcpool"
	tdlstorage "github.com/iyear/tdl/core/storage"
	rclonefs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"

	"github.com/tiennm99dev/telegram-exporter/internal/remote"
	"github.com/tiennm99dev/telegram-exporter/internal/report"
	"github.com/tiennm99dev/telegram-exporter/internal/tdlkv"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
	"github.com/tiennm99dev/telegram-exporter/internal/verify"
)

// verifyCmd reports whether a chat is fully archived on a remote.
//
// Exit 0 means complete, 1 means it ran and found work outstanding. Those are
// distinct on purpose: a driver needs to tell "nothing left to do" from "still
// incomplete" without parsing output.
func verifyCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	var (
		chat      = fs.String("c", "", "chat id, username, or t.me link (required)")
		remoteArg = fs.String("r", "", "rclone destination, e.g. pikpak:archive (required)")
		ns        = fs.String("n", "default", "tdl session namespace")
		dataDir   = fs.String("storage", tdlkv.DefaultDir(), "tdl bolt storage directory")
		delStale  = fs.Bool("delete-misnamed", false, "delete remote files stored under a superseded name")
		assumeYes = fs.Bool("y", false, "do not prompt before deleting")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *chat == "" || *remoteArg == "" {
		return fmt.Errorf("%w: -c CHAT and -r REMOTE:PATH are both required", errUsage)
	}

	ctx, err := remote.Init(ctx, remote.DefaultTunables())
	if err != nil {
		return err
	}
	dst, err := remote.Resolve(ctx, *remoteArg)
	if err != nil {
		return err
	}

	kv, err := tdlkv.Open(*dataDir, *ns)
	if err != nil {
		return err
	}
	defer func() { _ = kv.Close() }()

	sess, err := tgsource.New(ctx, tgsource.Options{KV: kv})
	if err != nil {
		return err
	}

	var (
		result verify.Report
		idx    *remote.Index
	)
	if err := sess.Run(ctx, func(ctx context.Context, pool dcpool.Pool) error {
		api := pool.Default(ctx)

		peer, err := tgsource.ResolveChat(ctx, tgsource.Manager(api, tdlstorage.NewPeers(kv)), *chat)
		if err != nil {
			return err
		}

		// The chat is walked first and the remote listed second, so the snapshot
		// is never older than the wanted set. The reverse order could report a
		// file absent that was uploaded while the walk was still running.
		fmt.Fprintf(os.Stderr, "reading %s\n", *chat)
		scan := report.NewTicker(os.Stderr, "messages read")
		var scanned int
		var items []tgsource.Item
		for it, err := range tgsource.Walk(ctx, api, peer, func(n int) { scanned = n; scan.Update(n) }) {
			if err != nil {
				return err
			}
			items = append(items, it)
		}

		scan.Done(scanned)

		fmt.Fprintf(os.Stderr, "indexing %s\n", dst.String())
		idxTick := report.NewTicker(os.Stderr, "objects listed")
		var listed int
		idx, err = remote.BuildIndex(ctx, dst, peer.ID(), func(n int) {
			listed = n
			idxTick.Update(n)
		})
		if err != nil {
			return err
		}
		idxTick.Done(listed)
		if dup := idx.Collisions(); len(dup) > 0 {
			// An ambiguous snapshot makes every verdict about these names
			// unreliable, so it is reported rather than silently resolved.
			fmt.Fprintf(os.Stderr, "warning: %d basename(s) appear at more than one path; "+
				"verdicts for them may flip between runs:\n", len(dup))
			for _, d := range dup[:min(5, len(dup))] {
				fmt.Fprintf(os.Stderr, "  %q\n", d)
			}
		}
		fmt.Fprintln(os.Stderr)

		result = verify.Check(items, idx)
		return nil
	}); err != nil {
		return err
	}

	out := bufio.NewWriter(os.Stdout)
	result.Write(out)
	if err := out.Flush(); err != nil {
		return err
	}

	if *delStale {
		if err := deleteMisnamed(ctx, dst, idx, result, *assumeYes); err != nil {
			return err
		}
	}

	if !result.Complete() {
		return fmt.Errorf("%w: %d file(s) still to fetch", errIncomplete, len(result.Todo()))
	}
	return nil
}

// deleteMisnamed removes stale copies left by an earlier naming scheme.
//
// Off by default and confirmed by default: this deletes data from the operator's
// remote, and a stale copy costs storage rather than correctness, so there is no
// hurry that justifies doing it unasked.
//
// Objects are addressed by the path the index recorded, not by the name matching
// used elsewhere. Those differ the moment a remote has directory structure, and
// deleting by basename would either miss the object or — worse, if the root
// happens to hold a same-named file — delete the wrong one.
func deleteMisnamed(ctx context.Context, dst rclonefs.Fs, idx *remote.Index, result verify.Report, assumeYes bool) error {
	var targets []string
	for _, m := range result.Misnamed {
		targets = append(targets, m.Found...)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "\nnothing to delete: no files stored under a superseded name")
		return nil
	}

	// Filenames are chosen by whoever uploaded the file and may contain control
	// characters or bidi marks, so they are quoted rather than printed raw: an
	// embedded newline or escape sequence could otherwise redraw this list and
	// have the operator approve something other than what they read.
	fmt.Fprintf(os.Stderr, "\nabout to delete %d file(s) from %s:\n", len(targets), dst.String())
	for _, t := range targets {
		fmt.Fprintf(os.Stderr, "  %q\n", t)
	}

	if !assumeYes {
		// Anything that is not an explicit yes leaves the files alone, and that
		// includes the read failing. Closed or non-interactive stdin — cron, a
		// pipeline — therefore declines rather than proceeding, which is the
		// safe direction for a delete.
		fmt.Fprint(os.Stderr, "delete these? [y/N] ")
		var answer string
		_, _ = fmt.Scanln(&answer)
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		default:
			fmt.Fprintln(os.Stderr, "left alone")
			return nil
		}
	}

	// One failure must not strand the rest: the operator approved a set, so the
	// whole set is attempted and the outcome reported as a count they can check
	// against what they approved.
	var errs []error
	deleted := 0
	for _, name := range targets {
		path, ok := idx.PathOf(name)
		if !ok {
			errs = append(errs, fmt.Errorf("%q is no longer in the index", name))
			continue
		}
		obj, err := dst.NewObject(ctx, path)
		if err != nil {
			errs = append(errs, fmt.Errorf("locate %q: %w", path, err))
			continue
		}
		if err := operations.DeleteFile(ctx, obj); err != nil {
			errs = append(errs, fmt.Errorf("delete %q: %w", path, err))
			continue
		}
		deleted++
	}

	fmt.Fprintf(os.Stderr, "deleted %d of %d\n", deleted, len(targets))
	return errors.Join(errs...)
}
