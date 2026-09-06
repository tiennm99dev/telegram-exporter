package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/iyear/tdl/core/dcpool"
	tdlstorage "github.com/iyear/tdl/core/storage"

	"github.com/tiennm99dev/telegram-exporter/internal/report"
	"github.com/tiennm99dev/telegram-exporter/internal/tdlkv"
	"github.com/tiennm99dev/telegram-exporter/internal/tgsource"
)

// listCmd prints every media message in a chat as `id<TAB>size<TAB>name`.
//
// It is the smallest thing that exercises the whole read path — resolve a chat,
// walk its history, derive a name — so a naming or paging problem shows up here
// rather than halfway through an archive run. The output is tab-separated, and
// the name is quoted: it comes from DocumentAttributeFilename, so whoever
// uploaded the file chose it, and a raw tab would shift the columns while a raw
// newline would split the record. Quoting also renders escape sequences inert
// rather than letting them redraw the operator's terminal.
func listCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var (
		chat    = fs.String("c", "", "chat id, username, or t.me link (required)")
		ns      = fs.String("n", "default", "tdl session namespace")
		dataDir = fs.String("storage", tdlkv.DefaultDir(), "tdl bolt storage directory")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *chat == "" {
		return fmt.Errorf("%w: -c CHAT is required", errUsage)
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

	return sess.Run(ctx, func(ctx context.Context, pool dcpool.Pool) error {
		api := pool.Default(ctx)

		peer, err := tgsource.ResolveChat(ctx, tgsource.Manager(api, tdlstorage.NewPeers(kv)), *chat)
		if err != nil {
			return err
		}

		// Buffered: one write syscall per item would dominate the runtime on a
		// chat with tens of thousands of messages. Flushed explicitly below so a
		// write failure — a closed pipe, a full disk — is reported rather than
		// swallowed by a deferred call nobody checks.
		out := bufio.NewWriter(os.Stdout)

		scan := report.NewTicker(os.Stderr, "messages read")
		count := 0
		var total int64
		for it, err := range tgsource.Walk(ctx, api, peer, scan.Update) {
			if err != nil {
				return err
			}
			count++
			total += it.Size()
			if _, err := fmt.Fprintf(out, "%d\t%d\t%q\n", it.MessageID, it.Size(), it.Name); err != nil {
				return err
			}
		}

		if err := out.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\n%d media messages, %.1f GiB\n", count, float64(total)/(1<<30))
		return nil
	})
}
