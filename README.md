# telegram-exporter — ⚠️ churned

**Status: churned as of 2026-08-25. No further development, no maintenance, no
issue support.** This repo now contains only these instructions; the retired
Python implementation lives in git history (see the last section).

## Why

[iyear/tdl](https://github.com/iyear/tdl) — an actively maintained Telegram
toolkit in Go, built on [gotd/td](https://github.com/gotd/td) — already covers
this project's purpose: export chat history and download media from DMs, groups
and channels over MTProto with a user account (so neither Bot API limit applies:
it can read history, and there is no 20 MB download cap). Maintaining a parallel
tool is not justified.

In short: a gotd-based rewrite was researched and judged feasible but not
worth it, since tdl already exists and is proven; the original project's
research and build reports are in git history at commit `3286ee2`.

## Use tdl instead

Install: see <https://docs.iyear.me/tdl/getting-started/installation/>.

```bash
tdl login                                   # user-account login (phone + code + 2FA)

# 1) export message metadata to JSON
tdl chat export -c @mygroup --all --with-content -o export.json

# 2) download the media it references
tdl dl -f export.json -d ./exports --takeout --group --skip-same --continue
```

Flag notes, mapped to what this project used to do:

- `--takeout` — takeout session with lower flood-wait limits for bulk export
  (an improvement this project never had).
- `--group` — detect albums (`grouped_id`) and download grouped messages together.
- `--skip-same` + `--continue` — resume: skip files matching existing name+size,
  continue interrupted downloads without prompting.
- `--all --with-content` — include text-only messages with their content
  (equivalent of `--include-text`).
- Filters: time range / message-id range via `-i`, plus expression filters, e.g.
  `-f "Media.Size > 5*1024*1024"`. Extension filters via `-i jpg,png` / `-e mp4`.
- tdl defaults to aggressive parallelism (`-t 8 -l 4`). If you hit flood waits
  on a large export, lower it (`-t 4 -l 1`).

What tdl does **not** replicate from this project's contract: per-post album
folders keyed on message id, the `messages.jsonl` append-only sidecar, the
cursor with filter-mismatch refusal, and the dry-run disk estimate. If you need
those guarantees, the old implementation is in this repo's history and `src/`.

## Special case: exporting to WebDAV

tdl can only write to a local directory — it has no WebDAV (or any remote)
destination. When the group's media is larger than local disk, use a **rolling
pipeline**: tdl downloads into a small staging directory while
[rclone](https://rclone.org/) concurrently moves (upload + delete local)
finished files to WebDAV. Local disk then only needs to hold the files
currently in flight plus one sync interval of throughput — Telegram caps a
single file at 2 GB (4 GB from premium uploaders), so a few dozen GB of staging
covers the worst case regardless of the group's total size.

One-time rclone remote setup:

```bash
rclone config create tg-webdav webdav \
  url=https://dav.example.com/remote.php/dav/files/you \
  vendor=other user=YOU pass=SECRET
```

Pipeline (bash):

```bash
tdl dl -f export.json -d ./staging --takeout --group --skip-same --continue &
TDL_PID=$!
while kill -0 "$TDL_PID" 2>/dev/null; do
  rclone move ./staging tg-webdav:tg-export --min-age 2m --delete-empty-src-dirs
  sleep 60
done
rclone move ./staging tg-webdav:tg-export --delete-empty-src-dirs   # final sweep
```

This repo ships that loop as **`tg-export-webdav.sh`**, hardened for unattended
runs:

```bash
./tg-export-webdav.sh -r tg-webdav:tg-export -c @mygroup      # export, then pipeline
./tg-export-webdav.sh -r tg-webdav:tg-export -- -t 4 -l 1     # pass flags to tdl dl
./tg-export-webdav.sh -h                                      # all options
```

What it adds over the loop above:

- Checks `tdl`, `rclone` and remote reachability before downloading anything.
- Excludes tdl's `*.tmp` partials from every sweep. tdl downloads to
  `<name>.tmp` and renames on completion, so a download stalled by a flood wait
  stops touching its `.tmp`; age alone would let rclone upload it half-written
  and destroy the resume point for that file.
- Defers `--delete-empty-src-dirs` to the final sweep, and recreates the staging
  dir after every sweep. rclone removing a directory under a running tdl makes
  tdl fail to create its next file.
- Stops `tdl` on Ctrl-C, `SIGTERM` or its own exit, so no download is orphaned,
  and propagates tdl's exit code (`130`/`143` interrupted, `3` rclone failure,
  `2` usage error).
- Runs the unrestricted final sweep **only** after tdl exits 0, and reports it
  loudly if it fails. An interrupted or crashed run gets the `--min-age`-guarded
  sweep instead and keeps the staging dir for resume.
- Aborts if rclone fails 5 sweeps in a row, instead of silently letting staging
  grow until the disk fills.

Pipeline (PowerShell):

```powershell
$tdl = Start-Process tdl -ArgumentList 'dl','-f','export.json','-d','./staging','--takeout','--group','--skip-same','--continue' -PassThru -NoNewWindow
while (-not $tdl.HasExited) {
  rclone move ./staging tg-webdav:tg-export --min-age 2m --delete-empty-src-dirs
  Start-Sleep -Seconds 60
}
rclone move ./staging tg-webdav:tg-export --delete-empty-src-dirs
```

Why it works:

- `--min-age 2m` keeps rclone away from files tdl is still writing; the final
  sweep after tdl exits catches everything else.
- **Add `--exclude '*.tmp'` to both `rclone move` calls in the loops above.**
  tdl writes `<name>.tmp` and renames on completion, so age is not a reliable
  completion signal: a download stalled by a flood wait stops touching its
  `.tmp`, and the loops as written will upload that partial file and delete the
  local copy, breaking `--continue` for it. `tg-export-webdav.sh` does this.
- Both legs are independently resumable: re-run tdl (`--skip-same --continue`)
  and re-run the rclone loop; nothing is downloaded or uploaded twice.
- Caveat: `--skip-same` compares name+size against the **staging** dir, which
  is empty after files move to WebDAV. Cross-run dedupe therefore rests on
  `--continue` (tdl's own completion tracking) — keep the same `export.json`
  between runs. If you restart with a fresh export, narrow it to the missing
  message-id range (`-T id -i <last>,<max>`) instead of re-downloading
  everything.

**Zero-staging streaming (no local disk at all) is not possible with tdl.** It
requires custom code that pipes download chunks straight into a WebDAV `PUT` —
both Telethon (`iter_download`) and gotd (`Stream(ctx, w)`) support that; see
the reports in `plans/reports/` at commit `3286ee2` if you ever need to build it.

## The retired implementation

A complete, tested (214 offline tests) Python/Telethon exporter with per-post
album folders, a `messages.jsonl` sidecar, resumable cursor semantics and a
dry-run size estimator lives at commit `3286ee2`:

```bash
git show 3286ee2:README.md              # its documentation
git checkout 3286ee2 -- src tests pyproject.toml requirements.txt   # resurrect it
```

It is unmaintained and pins Telethon `<2`; expect bit-rot.
