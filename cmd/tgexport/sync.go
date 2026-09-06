package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"iter"
	"os"

	"github.com/iyear/tdl/core/dcpool"
	tdlstorage "github.com/iyear/tdl/core/storage"
	"github.com/rclone/rclone/fs"

	"github.com/tiennm99dev/telegram-exporter/internal/pipeline"
	"github.com/tiennm99dev/telegram-exporter/internal/remote"
	"github.com/tiennm99dev/telegram-exporter/internal/report"
	"github.com/tiennm99dev/telegram-exporter/internal/tdlkv"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
	"github.com/tiennm99dev/telegram-exporter/internal/verify"
)

// retiredFlags map options the shell pipeline had onto what replaced them.
//
// Recognising them beats "flag provided but not defined": these were in
// muscle memory and in wrapper scripts, and a bare parse error does not say
// whether the concept moved or disappeared.
var retiredFlags = map[string]string{
	"i": "the rclone sweep interval is gone; uploads start the moment a download finishes",
	"a": "--min-age is gone; a file is only uploaded once the downloader reports it complete",
	"f": "the export JSON is gone; the chat is read live, so names cannot go stale",
	"p": "there are no passes; one invocation converges, and re-running resumes",
	"q": "renamed to --min-free",
}

// syncCmd archives a chat to a remote: read the chat, skip what is already
// there, download and upload the rest, then report on the result.
func syncCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	var (
		chat       = fs.String("c", "", "chat id, username, or t.me link (required)")
		remoteArg  = fs.String("r", "", "rclone destination, e.g. pikpak:archive (required)")
		staging    = fs.String("d", "./staging", "staging directory for files in flight")
		maxStaging = fs.String("m", "", "cap staging at this size, e.g. 40G (default: no cap)")
		threads    = fs.Int("threads", 4, "connections per file")
		limit      = fs.Int("limit", 2, "files downloading at once")
		uploads    = fs.Int("uploads", 2, "files uploading at once")
		minFree    = fs.Int64("min-free", 5, "stop if the remote has fewer than this many GiB free")
		limitItems = fs.Int("limit-items", 0, "stop after this many files (0 means no limit)")
		confirm    = fs.Bool("confirm", true, "re-state each uploaded file to prove its size")
		takeout    = fs.Bool("takeout", true, "use a takeout session, as `tdl dl --takeout` did")
		ns         = fs.String("n", "default", "tdl session namespace")
		dataDir    = fs.String("storage", tdlkv.DefaultDir(), "tdl bolt storage directory")
	)
	for name, replacement := range retiredFlags {
		fs.Var(retiredFlag{name, replacement}, name, "retired")
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *chat == "" || *remoteArg == "" {
		return fmt.Errorf("%w: -c CHAT and -r REMOTE:PATH are both required", errUsage)
	}

	budget, err := parseSize(*maxStaging)
	if err != nil {
		return fmt.Errorf("%w: -m %v", errUsage, err)
	}

	ctx, err = remote.Init(ctx, remote.DefaultTunables())
	if err != nil {
		return err
	}
	dst, err := remote.Resolve(ctx, *remoteArg)
	if err != nil {
		return err
	}

	// Partial files from an earlier run cannot be continued — core's downloader
	// takes no starting offset — so they are cleared before anything else fills
	// the disk with fragments no run will finish.
	if swept, err := pipeline.SweepPartials(*staging); err != nil {
		return fmt.Errorf("clear partial downloads: %w", err)
	} else if swept > 0 {
		fmt.Fprintf(os.Stderr, "cleared %d partial download(s) from an earlier run\n", swept)
	}

	// Before anything expensive: prove the destination is reachable and writable.
	if err := remote.EnsureDir(ctx, dst); err != nil {
		return err
	}
	if err := checkFree(ctx, dst, *minFree); err != nil {
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

	var final verify.Report
	if err := sess.Run(ctx, func(ctx context.Context, pool dcpool.Pool) error {
		api := pool.Default(ctx)

		peer, err := tgsource.ResolveChat(ctx, tgsource.Manager(api, tdlstorage.NewPeers(kv)), *chat)
		if err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "reading %s\n", *chat)
		var items []tgsource.Item
		for it, err := range tgsource.Walk(ctx, api, peer) {
			if err != nil {
				return err
			}
			items = append(items, it)
		}

		idx, err := remote.BuildIndex(ctx, dst, peer.ID())
		if err != nil {
			return err
		}
		before := verify.Check(items, idx)
		fmt.Fprintf(os.Stderr, "%d media messages, %d already archived, %d to fetch\n",
			before.Expected, before.Present, len(before.Todo()))

		todo := selectTodo(items, before, *limitItems)
		if len(todo) == 0 {
			final = before
			return nil
		}

		if err := validateBudget(budget, todo); err != nil {
			return err
		}

		var todoBytes int64
		for _, it := range todo {
			todoBytes += it.Size()
		}
		fmt.Fprintf(os.Stderr, "fetching %d file(s), %.1f GiB\n", len(todo), float64(todoBytes)/(1<<30))

		rep := report.New(os.Stderr, len(todo), todoBytes)
		res, runErr := pipeline.Run(ctx, sliceSeq(todo), pipeline.Options{
			Pool:    pool,
			Dst:     dst,
			Staging: *staging,
			Threads: *threads,
			Limit:   *limit,
			Uploads: *uploads,
			Budget:  budget,
			Confirm: *confirm,
			Takeout: *takeout,
			Report:  rep.Update,
		})
		rep.Finish(res.Stats)

		for _, f := range res.Failed() {
			fmt.Fprintf(os.Stderr, "  message %d failed: %v\n", f.Item.MessageID, f.Err)
		}
		if runErr != nil {
			return runErr
		}

		// The remote is re-indexed rather than assumed: the run's own view of
		// what it uploaded is exactly the thing under test.
		idx, err = remote.BuildIndex(ctx, dst, peer.ID())
		if err != nil {
			return err
		}
		final = verify.Check(items, idx)
		return nil
	}); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr)
	final.Write(os.Stdout)

	if !final.Complete() {
		return fmt.Errorf("%w: %d file(s) still to fetch", errIncomplete, len(final.Todo()))
	}
	return nil
}

// selectTodo picks the items still needing a fetch, newest first, optionally
// capped for a smoke test.
func selectTodo(items []tgsource.Item, r verify.Report, limit int) []tgsource.Item {
	want := make(map[int]struct{}, len(r.Todo()))
	for _, id := range r.Todo() {
		want[id] = struct{}{}
	}
	// Unsafe names are in Todo so they stay visible in the report, but fetching
	// one is impossible by definition, so it is not queued for download.
	for _, u := range r.Unsafe {
		delete(want, u.MessageID)
	}

	var todo []tgsource.Item
	for _, it := range items {
		if _, ok := want[it.MessageID]; !ok {
			continue
		}
		todo = append(todo, it)
		if limit > 0 && len(todo) == limit {
			break
		}
	}
	return todo
}

func sliceSeq(items []tgsource.Item) iter.Seq2[tgsource.Item, error] {
	return func(yield func(tgsource.Item, error) bool) {
		for _, it := range items {
			if !yield(it, nil) {
				return
			}
		}
	}
}

// validateBudget refuses a cap smaller than the largest file.
//
// The semaphore can never admit a weight above its limit, so such a run would
// block forever on a file it could never start — indistinguishable, from the
// outside, from a stalled remote. run.sh could only warn about this after the
// fact, once draining failed to get back under the cap.
func validateBudget(budget int64, todo []tgsource.Item) error {
	if budget <= 0 {
		return nil
	}
	var largest int64
	for _, it := range todo {
		if it.Size() > largest {
			largest = it.Size()
		}
	}
	if largest > budget {
		return fmt.Errorf("%w: -m is %.1f GiB but the largest file to fetch is %.1f GiB; "+
			"the cap must exceed the biggest single file",
			errUsage, float64(budget)/(1<<30), float64(largest)/(1<<30))
	}
	return nil
}

// checkFree refuses to start when the remote is nearly full.
//
// A backend that cannot report a quota is treated as unlimited rather than as a
// failure — the shell pipeline made that choice deliberately so a remote without
// an About API never blocked a run, and it is preserved.
func checkFree(ctx context.Context, dst fs.Fs, minGiB int64) error {
	free, ok := remote.FreeBytes(ctx, dst)
	if !ok {
		return nil
	}
	freeGiB := free / (1 << 30)
	fmt.Fprintf(os.Stderr, "%s has %d GiB free\n", dst.String(), freeGiB)
	if freeGiB < minGiB {
		return fmt.Errorf("%s has only %d GiB free, below the %d GiB floor; "+
			"free space or lower --min-free", dst.String(), freeGiB, minGiB)
	}
	return nil
}

// retiredFlag reports a helpful error for an option that no longer exists.
type retiredFlag struct{ name, replacement string }

func (r retiredFlag) String() string { return "" }
func (r retiredFlag) Set(string) error {
	return fmt.Errorf("-%s no longer exists: %s", r.name, r.replacement)
}

// parseSize reads a binary size such as 40G, matching what run.sh -m accepted.
func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch unit := s[len(s)-1]; unit {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	default:
		if unit < '0' || unit > '9' {
			return 0, fmt.Errorf("unknown size suffix %q, expected K, M, G or T", string(unit))
		}
	}
	digits := s
	if mult > 1 {
		digits = s[:len(s)-1]
	}

	var n int64
	if digits == "" {
		return 0, fmt.Errorf("%q has no number", s)
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a size", s)
		}
		n = n*10 + int64(r-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be greater than zero")
	}
	return n * mult, nil
}
