---
name: rclone-embedding-traps
description: Non-obvious rclone-as-a-library behaviours verified against rclone v1.75.1 source that keep biting this repo's remote/index code
metadata:
  type: project
---

rclone v1.75.1 embedded as a library reads `RCLONE_*` environment variables into
package-level global option structs at **package init**, via
`fs.RegisterGlobalOptions` -> `OptionsInfo.load()` -> `fs.ConfigMap` ->
`optionEnvVars.Get` (`fs/registry.go:498-527`, `fs/configmap.go:115-152`). No
cobra/pflag wiring is needed for this to happen.

Two consequences that are easy to get backwards, both verified empirically:

- `filter.NewFilter(nil)` copies `filter.Opt` (`fs/filter/filter.go:196-202`),
  which already holds the env-derived values. It is **not** a neutral filter.
  `RCLONE_EXCLUDE` / `RCLONE_FILTER` / `RCLONE_MIN_SIZE` / `RCLONE_MAX_AGE` all
  survive it. A genuinely neutral filter needs explicit
  `&filter.Options{MinAge: fs.DurationOff, MaxAge: fs.DurationOff, MinSize: -1, MaxSize: -1}`
  — the zero-value `filter.Options{}` errors with "min-age can't be larger than
  max-age".
- `fs.AddConfig(ctx)` + assigning `ci.MaxDepth` *does* override
  `RCLONE_MAX_DEPTH`, and env values for `Transfers` / `LowLevelRetries` really
  are present in `fs.GetConfig(ctx)` without any flag parsing.

**Why:** an index built through a narrowed listing does not fail — it reports
archived files as absent and re-downloads them, which is the exact multi-GiB
failure this project exists to remove.

**How to apply:** when reviewing anything that lists a remote, check the filter
is neutralised explicitly rather than with `NewFilter(nil)`, and prove it with a
subprocess run under the env var rather than a `t.Setenv` test — `t.Setenv` runs
long after rclone's package init, so such a test passes regardless.

Related: [[repo-exit-code-contract]]
