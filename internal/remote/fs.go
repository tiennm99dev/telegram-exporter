// Package remote owns the rclone side: resolving a destination and reporting on it.
package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configfile"
)

// Tunables are rclone settings this tool overrides.
//
// The values exist because of pikpak: it commits an upload as a server-side
// async task, and rclone abandons a still-pending one once its low-level retries
// run out, failing a transfer that would have succeeded. Fewer parallel
// transfers keep that queue short; more retries wait it out.
type Tunables struct {
	Transfers       int
	LowLevelRetries int
}

// DefaultTunables are the values the shell pipeline settled on for pikpak.
func DefaultTunables() Tunables {
	return Tunables{Transfers: 2, LowLevelRetries: 20}
}

// installOnce guards configfile.Install, which swaps unsynchronised package
// globals in rclone's config package. Calling it twice is harmless on its own,
// but racing it against an Fs resolution is not.
var installOnce sync.Once

// Init loads the user's rclone.conf and applies tunables to a derived context.
//
// Environment variables still win: rclone reads RCLONE_TRANSFERS and friends
// into its global config at package init, and fs.AddConfig copies that, so
// skipping the assignment when the variable is set preserves the operator's
// value.
//
// The config is loaded here, explicitly, because rclone's lazy path is fatal:
// config.LoadedData() calls fs.Fatalf on a config file it cannot parse or
// decrypt, and fs.Fatalf calls os.Exit(1) — past every defer, and with an exit
// code this tool defines as "incomplete", which would send a driver into an
// endless retry. Loading up front turns that into an ordinary error.
func Init(ctx context.Context, t Tunables) (context.Context, error) {
	var err error
	installOnce.Do(func() {
		configfile.Install()
		if lerr := config.Data().Load(); lerr != nil && !errors.Is(lerr, config.ErrorConfigFileNotFound) {
			err = fmt.Errorf("cannot read rclone config %q: %w "+
				"(an encrypted config needs RCLONE_CONFIG_PASS)", config.GetConfigPath(), lerr)
		}
	})
	if err != nil {
		return ctx, err
	}

	ctx, ci := fs.AddConfig(ctx)
	if !envSet("RCLONE_TRANSFERS") && t.Transfers > 0 {
		ci.Transfers = t.Transfers
	}
	if !envSet("RCLONE_LOW_LEVEL_RETRIES") && t.LowLevelRetries > 0 {
		ci.LowLevelRetries = t.LowLevelRetries
	}
	return ctx, nil
}

// Resolve opens a destination given as an rclone REMOTE:PATH.
//
// rclone itself is the authority on what resolves: a remote can come from
// rclone.conf, from RCLONE_CONFIG_<NAME>_* environment variables with no config
// entry at all, from an inline `:type,opt=val:` connection string, or from a
// parameterised name like `pikpak,chunk_size=10M:path`. Pre-screening the name
// against the config sections would reject the last three, so the call is made
// first and the friendly "here is what you have configured" message is produced
// only for the one error that means the name was never defined.
func Resolve(ctx context.Context, remote string) (fs.Fs, error) {
	if remote == "" {
		return nil, fmt.Errorf("a destination remote is required (REMOTE:PATH)")
	}
	if !strings.Contains(remote, ":") {
		return nil, fmt.Errorf("remote %q is not in rclone REMOTE:PATH form", remote)
	}

	f, err := fs.NewFs(ctx, remote)
	if err != nil {
		if errors.Is(err, fs.ErrorNotFoundInConfigFile) {
			return nil, fmt.Errorf("rclone remote %q is not configured; configured remotes: %s",
				remote, strings.Join(sections(), ", "))
		}
		return nil, fmt.Errorf("cannot reach %q — check credentials and connectivity: %w", remote, err)
	}
	return f, nil
}

// FreeBytes reports free space on the remote.
//
// Backends without quota reporting return ok=false rather than an error: the
// shell pipeline treated an unanswerable quota as "unlimited" so a backend that
// cannot report never blocks a run, and that behaviour is preserved.
func FreeBytes(ctx context.Context, f fs.Fs) (free int64, ok bool) {
	about := f.Features().About
	if about == nil {
		return 0, false
	}
	usage, err := about(ctx)
	if err != nil || usage == nil || usage.Free == nil {
		return 0, false
	}
	return *usage.Free, true
}

// sections lists configured remote names. Safe only after Init has loaded the
// config without error.
func sections() []string {
	out := config.FileSections()
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}

func envSet(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}
