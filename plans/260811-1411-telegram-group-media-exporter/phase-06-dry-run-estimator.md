---
phase: 6
title: "Dry-Run Estimator"
status: complete
priority: P2
dependencies: [5]
effort: "~1h"
---

# Phase 6: Dry-Run Estimator

## Overview

`--dry-run`: sweep, download nothing, report what a real run would fetch **from where it would actually start**. With 41 GB free and an unknown group size, this is the difference between a planned export and a surprise.

## Requirements

- Functional: counts and bytes by media type honoring the same filters *and the same cursor*; disk comparison; non-zero exit when it won't fit
- Non-functional: no `download_media` calls; no writes anywhere, including no creation of the export root; no lock taken; no state mutation

## Architecture

`run_estimate()` drives the **same** `iter_posts` generator with the same `Filters` as `run_download()`. That shared generator is the anti-drift property — an estimator with its own traversal would report numbers that don't match reality. There is no `Sink` Protocol: both run modes own a plain `async for`, and the duplication is two lines.

### The cursor fix

Dry-run **reads** state (never writes) and starts where the real run would:

```python
state = State.load(out_dir)                     # read-only
start = state.cursor_id if state and state.filters == filters else 0
```

Without this, `tg-export --dry-run && tg-export` fails **closed** on every resumed run: a 38 GB group with 30 GB already downloaded and 11 GB free would re-measure the whole 38 GB, exit 2, and refuse to fetch the 8 GB that fits comfortably. The user's only escape would be to bypass the guard they were told to trust.

Deliberately **not** doing a "scan the export dir and subtract bytes on disk" pass: with a cursor-based sweep everything before the cursor is already excluded, and the residue is a handful of failed files and one partial post. A disk-scan reconciliation is unearned complexity.

### Report

```
Group:      My Group  (id -1001234567890)
Filters:    types=video,photo  since=2026-01-01  max-size=100MB
Mode:       resume from message 1042            # or "full history (no prior state)"

Remaining:  1,204 posts / 3,891 files
  photos      1,902 files    2.1 GiB   (approximate — size variant)
  videos      1,944 files   35.4 GiB
  documents      45 files    0.7 GiB
  unknown size    3 files    (not counted)

Total:      38.2 GiB
Free:       11.0 GiB   Reserve: 2.0 GiB
Verdict:    SHORT by 29.2 GiB                    exit 2
```

Unknown-size files get their own line rather than being coerced to zero — an unbounded estimate error must be visible. Photo totals are labelled approximate; document/video/audio are exact.

Posts and files count **posts with files** — neither empty posts (fully filtered groups, which phase 3 yields to keep the cursor advancing) nor text-only posts under `--include-text` are counted, on both sides of any comparison. Text messages carry no bytes, so the estimate is unaffected by the flag.

<!-- Updated: Validation Session 1 - --include-text does not affect posts_with_files counting -->


### The verdict is advisory; the per-file guard enforces

Phase 5's per-file `check_disk` is the sole enforcement mechanism. This removes the previously "optional preflight behind a flag" — a flag that appeared in no flag surface — and the double sweep it implied on huge groups.

Say this plainly in the README: a `SHORT` verdict does not mean the run is unsafe. It means the run will stop partway with a clean, resumable cursor.

## Related Code Files

- Create: `src/telegram_exporter/estimate.py`
- Modify: `src/telegram_exporter/cli.py` (`--dry-run` dispatches to `run_estimate`)

## Implementation Steps

1. `EstimateSink` accumulating posts-with-files, files, and bytes by media kind, plus an unknown-size tally
2. Human-readable byte formatting (GiB/MiB, thousands separators)
3. `State.load` in read-only mode; derive `start` and the `Mode:` line
4. Free space via `shutil.disk_usage(out_dir)`; compare `total + reserve` against free
5. Report renderer per the layout; label photo totals approximate
6. `cli.py`: `--dry-run` → `run_estimate`, taking no lock and creating no directories
7. Verify dry-run leaves the filesystem untouched

## Success Criteria

- [ ] `--dry-run` completes with zero `download_media` calls and zero bytes written
- [ ] Dry-run does not create the export root, `.export-state.json`, `messages.jsonl`, or `.export.lock`
- [ ] Dry-run takes no lock — it can run while a real export is in progress
- [ ] On a resumed export, `Mode:` shows the cursor and totals cover only remaining work
- [ ] `tg-export --dry-run && tg-export` succeeds on a partially completed export that fits
- [ ] Totals broken down by media kind; unknown-size files on their own line, never as zero
- [ ] Exit `2` when total + reserve exceeds free space; `0` otherwise
- [ ] Filters applied identically to a real run via the shared `iter_posts`, not by duplicated logic
- [ ] `--dry-run --limit 20` vs `--limit 20` from the same start: post and file counts **exactly equal**, bytes within **2%** (photo approximation only)

## Risk Assessment

- **Estimator drifts from downloader** → the number users trust becomes a lie. Prevented structurally by the shared `iter_posts`; never fork the traversal.
- **Dry-run mutates state** → a later real run resumes from a cursor nothing downloaded. Read-only by construction; asserted.
- **Dry-run fails closed on resume** → fixed by reading the cursor. This was the defect that made the documented safe idiom unusable.
- **Photo approximation misread as exact** → labelled, plus the 2% tolerance is stated rather than left to judgement.
- **Sweeping a very large history triggers flood waits** → history reads are cheap and covered by `aiter_with_flood_retry`.
