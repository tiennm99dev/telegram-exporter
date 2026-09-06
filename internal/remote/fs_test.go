package remote

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
)

// Resolve's own validation runs before rclone is consulted, so these cases are
// checkable without a config file or a network.
func TestResolveRejectsMalformedDestinations(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{"empty", "", "destination remote is required"},
		{"no colon", "pikpak", "not in rclone REMOTE:PATH form"},
		{"path only", "/tmp/staging", "not in rclone REMOTE:PATH form"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Resolve(context.Background(), tt.remote)
			if err == nil {
				t.Fatalf("Resolve(%q) succeeded, want an error", tt.remote)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Resolve(%q) error = %v, want it to mention %q", tt.remote, err, tt.want)
			}
		})
	}
}

// Forms rclone accepts must not be rejected by our own pre-checks. An earlier
// version screened the name against the config file's sections, which turned
// away env-defined remotes, connection strings, and parameterised names that
// rclone resolves perfectly well. These must get past our validation and fail —
// if at all — inside rclone, on their own merits.
func TestResolveDefersUnusualFormsToRclone(t *testing.T) {
	forms := []string{
		"pikpak,chunk_size=10M:mychannel", // parameterised remote name
		":local:/tmp",                     // inline connection string
		"envonly:bucket",                  // possibly defined by RCLONE_CONFIG_ENVONLY_*
	}

	for _, form := range forms {
		t.Run(form, func(t *testing.T) {
			_, err := Resolve(context.Background(), form)
			if err == nil {
				return // rclone resolved it; nothing to assert
			}
			if strings.Contains(err.Error(), "not in rclone REMOTE:PATH form") {
				t.Errorf("Resolve(%q) was rejected by our own syntax check; "+
					"rclone should be the authority on what resolves: %v", form, err)
			}
		})
	}
}

func TestDefaultTunablesMatchPikpakSettings(t *testing.T) {
	// These two numbers are the outcome of debugging pikpak's async-commit
	// behaviour in the shell pipeline; a silent change would reintroduce
	// transfers that fail while the server is still committing.
	got := DefaultTunables()
	if got.Transfers != 2 {
		t.Errorf("Transfers = %d, want 2", got.Transfers)
	}
	if got.LowLevelRetries != 20 {
		t.Errorf("LowLevelRetries = %d, want 20", got.LowLevelRetries)
	}
}

func TestInitAppliesTunables(t *testing.T) {
	// Unset both so the defaults, not an operator override, are what lands.
	unset(t, "RCLONE_TRANSFERS")
	unset(t, "RCLONE_LOW_LEVEL_RETRIES")

	ctx, err := Init(context.Background(), Tunables{Transfers: 2, LowLevelRetries: 20})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	ci := fs.GetConfig(ctx)
	if ci.Transfers != 2 {
		t.Errorf("ctx Transfers = %d, want 2", ci.Transfers)
	}
	if ci.LowLevelRetries != 20 {
		t.Errorf("ctx LowLevelRetries = %d, want 20", ci.LowLevelRetries)
	}
}

// An operator's environment override must survive Init, so a tunable is only
// applied when the corresponding variable is absent.
func TestInitLeavesEnvOverridesAlone(t *testing.T) {
	unset(t, "RCLONE_TRANSFERS")
	t.Setenv("RCLONE_TRANSFERS", "7")

	ctx, err := Init(context.Background(), Tunables{Transfers: 2, LowLevelRetries: 20})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := fs.GetConfig(ctx).Transfers; got == 2 {
		t.Errorf("Transfers = 2; Init overwrote the RCLONE_TRANSFERS override")
	}
}

// unset removes a variable for the duration of the test, restoring it after.
func unset(t *testing.T, key string) {
	t.Helper()
	if old, ok := os.LookupEnv(key); ok {
		t.Cleanup(func() { _ = os.Setenv(key, old) })
	}
	_ = os.Unsetenv(key)
}
