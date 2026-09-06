package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/iyear/tdl/core/dcpool"

	"github.com/tiennm99dev/telegram-exporter/internal/remote"
	"github.com/tiennm99dev/telegram-exporter/internal/tdlkv"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// doctorCmd reports whether the two halves this tool depends on are usable: an
// authorised tdl session, and a reachable destination remote. It is the cheapest
// way to separate "misconfigured" from "broken" before starting a long run.
//
// Both checks always run, so one broken half does not hide the state of the
// other, but their errors are returned rather than merely printed — a check
// tool that exits 0 on failure is worse than no check tool.
func doctorCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var (
		remoteArg = fs.String("r", "", "rclone destination `REMOTE:PATH` to check, e.g. pikpak:archive")
		ns        = fs.String("n", "default", "tdl session `NAMESPACE`")
		dataDir   = fs.String("storage", tdlkv.DefaultDir(), "`DIR` holding the tdl session store")
	)

	commandUsage(fs, "tgexport doctor [-r REMOTE:PATH] [options]",
		"Check the Telegram session and, when a remote is given, that it\nresolves and has free space. Run this before a long archive.")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err // main maps this to a clean exit
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	fmt.Printf("versions\n")
	for _, dep := range []string{
		"github.com/iyear/tdl/core",
		"github.com/rclone/rclone",
		"github.com/gotd/td",
	} {
		fmt.Printf("  %-28s %s\n", dep, moduleVersion(dep))
	}

	var problems []error

	fmt.Printf("\ntelegram\n")
	if err := checkTelegram(ctx, *dataDir, *ns); err != nil {
		fmt.Printf("  session   FAILED: %v\n", err)
		problems = append(problems, fmt.Errorf("telegram: %w", err))
	}

	fmt.Printf("\nremote\n")
	if *remoteArg == "" {
		fmt.Printf("  skipped   (pass -r REMOTE:PATH to check a destination)\n")
	} else if err := checkRemote(ctx, *remoteArg); err != nil {
		fmt.Printf("  %-9s FAILED: %v\n", *remoteArg, err)
		problems = append(problems, fmt.Errorf("remote: %w", err))
	}

	return errors.Join(problems...)
}

func checkTelegram(ctx context.Context, dataDir, ns string) error {
	kv, err := tdlkv.Open(dataDir, ns)
	if err != nil {
		return err
	}
	defer func() { _ = kv.Close() }()

	fmt.Printf("  store     %s (namespace %q)\n", dataDir, ns)

	sess, err := tgsource.New(ctx, tgsource.Options{KV: kv})
	if err != nil {
		return err
	}

	return sess.Run(ctx, func(ctx context.Context, _ dcpool.Pool) error {
		self, err := sess.Client().Self(ctx)
		if err != nil {
			return fmt.Errorf("fetch self: %w", err)
		}
		fmt.Printf("  account   %s (id %d)\n", describeUser(self), self.ID)
		return nil
	})
}

func checkRemote(ctx context.Context, dest string) error {
	ctx, err := remote.Init(ctx, remote.DefaultTunables())
	if err != nil {
		return err
	}

	f, err := remote.Resolve(ctx, dest)
	if err != nil {
		return err
	}
	fmt.Printf("  resolved  %s\n", f.String())

	if free, ok := remote.FreeBytes(ctx, f); ok {
		fmt.Printf("  free      %.1f GiB\n", float64(free)/(1<<30))
	} else {
		// Not a failure: several backends have no quota API, and the shell
		// pipeline deliberately treated that as unlimited rather than blocking.
		fmt.Printf("  free      not reported by this backend (treated as unlimited)\n")
	}
	return nil
}

func describeUser(u *tg.User) string {
	parts := make([]string, 0, 3)
	if u.FirstName != "" {
		parts = append(parts, u.FirstName)
	}
	if u.LastName != "" {
		parts = append(parts, u.LastName)
	}
	if u.Username != "" {
		parts = append(parts, "@"+u.Username)
	}
	if len(parts) == 0 {
		return "(unnamed)"
	}
	return strings.Join(parts, " ")
}

// moduleVersion reports the version a dependency was built against, read from
// the binary itself so it cannot drift from what is actually linked in.
func moduleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range info.Deps {
		if d.Path == path {
			return d.Version
		}
	}
	return "not linked"
}
