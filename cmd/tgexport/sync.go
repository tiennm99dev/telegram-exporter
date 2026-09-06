package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"iter"
	"math"
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

// syncCmd archives a chat to a remote: read the chat, skip what is already
// there, download and upload the rest, then report on the result.
func syncCmd(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	var (
		chat       = flags.String("c", "", "`CHAT`: id, username, or t.me link (required)")
		remoteArg  = flags.String("r", "", "rclone destination `REMOTE:PATH`, e.g. pikpak:archive (required)")
		staging    = flags.String("d", "./staging", "staging `DIR` for files in flight")
		maxStaging = flags.String("m", "", "cap staging at `SIZE`, e.g. 40G (default: no cap)")
		threads    = flags.Int("threads", 4, "connections per file")
		limit      = flags.Int("limit", 2, "files downloading at once")
		uploads    = flags.Int("uploads", 2, "files uploading at once")
		minFree    = flags.Int64("min-free", 5, "stop when the remote has under this many `GiB` free")
		limitItems = flags.Int("limit-items", 0, "stop after this many files (0 means no limit)")
		confirm    = flags.Bool("confirm", true, "re-state each uploaded file to prove its size")
		takeout    = flags.Bool("takeout", true, "use a takeout session for higher rate limits")
		ns         = flags.String("n", "default", "tdl session `NAMESPACE`")
		dataDir    = flags.String("storage", tdlkv.DefaultDir(), "`DIR` holding the tdl session store")
	)
	commandUsage(flags, "tgexport sync -c CHAT -r REMOTE:PATH [options]",
		"Archive a chat's media to a remote, fetching only what is missing.\n"+
			"Re-running resumes: anything already on the remote is skipped.")

	if err := flags.Parse(args); err != nil {
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

	var (
		final  verify.Report
		runErr error
	)
	if err := sess.Run(ctx, func(ctx context.Context, pool dcpool.Pool) error {
		api := pool.Default(ctx)

		peer, err := tgsource.ResolveChat(ctx, tgsource.Manager(api, tdlstorage.NewPeers(kv)), *chat)
		if err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "reading %s\n", *chat)
		scan := report.NewTicker(os.Stderr, "messages read")
		var items []tgsource.Item
		var scanned int
		for it, err := range tgsource.Walk(ctx, api, peer, func(n int) {
			scanned = n
			scan.Update(n)
		}) {
			if err != nil {
				return err
			}
			items = append(items, it)
		}
		scan.Done(scanned)

		idx, err := indexRemote(ctx, dst, peer.ID())
		if err != nil {
			return err
		}
		warnCollisions(idx)
		before := verify.Check(items, idx)
		report.Survey(os.Stderr, before)

		todo := selectTodo(items, before, *limitItems)
		if len(todo) == 0 {
			final = before
			return nil
		}

		if err := validateBudget(budget, todo); err != nil {
			return err
		}

		var todoBytes, largest int64
		for _, it := range todo {
			todoBytes += it.Size()
			largest = max(largest, it.Size())
		}
		report.Plan(os.Stderr, report.PlanInfo{
			Files:       len(todo),
			Bytes:       todoBytes,
			Largest:     largest,
			Budget:      budget,
			Staging:     *staging,
			Threads:     *threads,
			Downloads:   *limit,
			Uploads:     *uploads,
			Destination: dst.String(),
		})

		rep := report.Events(os.Stderr, len(todo), todoBytes)
		// rclone logs to stderr on its own schedule, which lands in the middle
		// of a bar redraw. Routing it through the renderer keeps both readable.
		if live, ok := rep.(*report.Live); ok {
			defer report.CaptureRcloneLog(ctx, live.LogWriter())()
		}
		var res pipeline.Result
		res, runErr = pipeline.Run(ctx, sliceSeq(todo), pipeline.Options{
			Pool:    pool,
			Dst:     dst,
			Staging: *staging,
			Threads: *threads,
			Limit:   *limit,
			Uploads: *uploads,
			Budget:  budget,
			Confirm: *confirm,
			Takeout: *takeout,
			Events:  rep,
			// Re-checked during the run, not only before it: an archive of this
			// size runs for hours, and the destination can fill in the middle.
			FreeBytes: func(ctx context.Context) (int64, bool) {
				return remote.FreeBytes(ctx, dst)
			},
			MinFree: *minFree * (1 << 30),
		})
		rep.Finish(res.Stats)

		for _, f := range res.Failed() {
			fmt.Fprintf(os.Stderr, "  message %d failed: %v\n", f.Item.MessageID, f.Err)
		}
		if runErr != nil {
			// Reported, not returned yet: a run that archived thousands of files
			// and hit one transient upload error has still made progress, and
			// suppressing the report would leave the operator — and any driver
			// reading the exit code — unable to tell that from a total failure.
			// It is returned after the report, below, so the exit code is right.
			fmt.Fprintf(os.Stderr, "run ended early: %v\n", runErr)
		}

		// The remote is re-indexed rather than assumed: the run's own view of
		// what it uploaded is exactly the thing under test.
		idx, err = indexRemote(ctx, dst, peer.ID())
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

	switch {
	case errors.Is(runErr, pipeline.ErrDestinationFailing):
		// Exit 3, not 1. The destination refused upload after upload, and it
		// will refuse them next pass too — a driver retrying on "incomplete"
		// would walk 18k messages and re-download gigabytes into a remote that
		// cannot take a byte, indefinitely.
		return runErr
	case final.Stalled():
		return fmt.Errorf("%w: %d file(s) remain, none of which can be fetched",
			errStalled, len(final.Todo()))
	case !final.Complete():
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

// indexRemote lists the destination, reporting progress as it goes.
func indexRemote(ctx context.Context, dst fs.Fs, dialogID int64) (*remote.Index, error) {
	fmt.Fprintf(os.Stderr, "indexing %s\n", dst.String())
	tick := report.NewTicker(os.Stderr, "objects listed")
	var seen int
	idx, err := remote.BuildIndex(ctx, dst, dialogID, func(n int) {
		seen = n
		tick.Update(n)
	})
	if err != nil {
		return nil, err
	}
	tick.Done(seen)
	return idx, nil
}

// warnCollisions reports basenames the index found at more than one path.
//
// verify printed this and sync did not, which was backwards: an ambiguous
// snapshot makes the presence verdict for those names unreliable, and sync is
// the command that acts on the verdict by moving data.
func warnCollisions(idx *remote.Index) {
	dup := idx.Collisions()
	if len(dup) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: %d basename(s) appear at more than one path; "+
		"verdicts for them may flip between runs:\n", len(dup))
	for _, name := range dup[:min(5, len(dup))] {
		fmt.Fprintf(os.Stderr, "    %q\n", name)
	}
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
		// Checked rather than allowed to wrap. A wrapped value is negative or
		// zero, and both mean "no cap" downstream — so a typo would silently
		// remove the staging limit instead of being refused.
		if n > (math.MaxInt64-int64(r-'0'))/10 {
			return 0, fmt.Errorf("%q is too large", s)
		}
		n = n*10 + int64(r-'0')
	}
	if n <= 0 {
		return 0, fmt.Errorf("must be greater than zero")
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return n * mult, nil
}
