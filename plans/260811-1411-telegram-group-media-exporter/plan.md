---
title: "Telegram Group Media Exporter"
description: "CLI tool exporting all media from a Telegram group to local disk, grouped by message/album, with resume, filters, and dry-run size estimation."
status: in-progress
priority: P2
branch: "main"
tags: [telethon, python, cli, mtproto, export]
blockedBy: []
blocks: []
created: "2026-08-11T07:22:18.583Z"
createdBy: "ck:plan"
source: skill
---

# Telegram Group Media Exporter

## Overview

CLI tool `tg-export` — downloads all media from a Telegram group to local disk, one folder per logical post (albums collapsed), with a `messages.jsonl` sidecar preserving message context. Resumable, filterable, with a dry-run size estimate.

Greenfield repo. Research: [research report](../reports/research-telethon-mtproto-260811-1411-telegram-group-media-exporter-report.md). Reviewed adversarially — see [Red Team Review](#red-team-review); the design below is post-review.

## Locked Decisions

Settled before planning — do not re-litigate without new evidence.

| Decision | Rationale |
|---|---|
| Telethon + Python 3.12, MTProto **user account** | Bot API is structurally incapable: no history access, 20 MB download cap |
| Sequential downloads, **concurrency 1** | Telethon does not parallelize transfers; parallelism accelerates FloodWait escalation (7s → 4h+) |
| Album-aware folders, id = **lowest message id** in the group | User decision; deterministic across re-runs |
| `SQLiteSession` file (not StringSession) | Persistent local filesystem; StringSession solves a containerization problem we don't have |
| `argparse` (not Typer/Click) | One command, ~12 flags, no subcommands |
| `pyproject.toml` + `[project.scripts]` | Current packaging standard |
| Telethon pinned `>=1.41,<2` + `requirements.txt` | Telethon 2.0 is alpha with a renamed API surface; the pin file records the version phase 1 actually verified |

## Architecture

Both run modes drive the **same** `iter_posts` generator with the same `Filters`. That shared generator — not any class hierarchy — is what guarantees the dry-run estimate describes what a real run would download. A forked traversal would drift and lie.

```
cli.py ──> session.py ──> traversal.py ──┬──> run_download()  ──> downloader.py, state.py, sidecar.py
 argparse   auth, flood     sweep +      │
 umask      wrapper         album group  └──> run_estimate()  ──> estimate.py
 logging                    + filters
 locks                                        paths.py  (shared, security-critical)
```

```
src/telegram_exporter/
  cli.py          argparse, umask, logging, locks, git-safety asserts, dispatch
  session.py      credentials, session path prep, client, flood retry, entity resolution
  traversal.py    Post, Filters, media_size(), iter_posts, album-split tripwire
  paths.py        export_root, post_dir, sanitize, derive_filename, safe_join   (pure)
  downloader.py   download_one, error taxonomy, .part sweep, run_download()
  state.py        resume cursor only
  sidecar.py      messages.jsonl append writer only
  estimate.py     EstimateSink, byte formatting, run_estimate()
tests/
  test_paths.py              sanitize table, safe_join, export_root type guard
  test_download_decisions.py skip/re-download/.part logic against a tmpdir
  test_traversal.py          album grouping + split tripwire fires
  test_state.py              filter mismatch, atomic save
  test_lock.py               subprocess contention
  test_logging.py            telethon logger stays >= WARNING
  test_no_concurrency.py     AST scan rejects gather/create_task
```

**No `Sink` Protocol.** Two implementations with one call site and no shared behavior is indirection describing a `for` loop. `run_download()` and `run_estimate()` each own their own `async for`. The anti-drift property lives in the shared `iter_posts` — restated here so nobody re-adds the Protocol believing it was load-bearing.

**`state.py` and `sidecar.py` are separate modules on purpose.** The correctness-critical part is the *ordering* between them, and that ordering belongs to the caller. Two objects in two modules makes the sequence visible at one call site and makes a "helpful" `commit_and_append()` — which would destroy Invariant 2 — impossible to write without noticing.

Output layout:

```
exports/g-1001234567890/     <- directory name is the chat id, never the group title
  title.txt                  <- human-readable name lives here, as data
  1042/                      <- album (shared grouped_id), folder = first member id
    1042_photo.jpg           <- filename keyed on message id, not a positional index
    1043_photo.jpg
    1044_video.mp4
  1047/                      <- standalone message
    1047_document.pdf
  messages.jsonl
  .export-state.json
  .export.lock
```

**Exactly one untrusted string ever becomes a path component: the filename leaf.** Every other component is an `int` enforced by a type check. The group title is server-supplied and editable by any admin, so it is stored as data and never as a path — which also stops a group rename from orphaning an in-progress export.

## Three Invariants

Violating any of these produces silent corruption that looks like success.

1. **Identity is filter-invariant.** The folder is the lowest message id in the *full* `grouped_id` group; the filename is keyed on `message_id`. Neither depends on which members a filter dropped, so `--types photo` and `--types video` address the same files.
2. **The cursor commits only after a complete post, and never from an abort path.** `state.commit()` is unreachable from every error exit, so the cursor can never point past an in-flight post.
3. **Filters are stored verbatim in the state file.** The cursor means "handled through here *under these filters*". A mismatch refuses the run and names what changed.

## Acceptance Criteria

`[x]` = verified offline by the test suite. `[~]` = implemented and unit-tested,
but the end-to-end proof is phase 7 against a live group.

- [~] `tg-export --group <g> --out ./exports --dry-run` reports remaining posts/files/bytes by media type and a disk verdict, writing nothing — *"writes nothing" and the per-kind breakdown are tested; the live sweep is phase 7 step 1*
- [~] `tg-export --group <g> --out ./exports` downloads all media into album-aware folders
- [~] Killing the run mid-export and re-running resumes with no re-downloads and no gaps — *dedupe, cursor and `.part` sweep are tested; the `kill -9` proof is phase 7 step 5*
- [x] `messages.jsonl` links every file to its message id, post id, date, sender, and caption
- [x] A message whose filename is `../../../etc/passwd` writes inside the post dir, proven by test
- [x] A group titled `..` or `a/b` cannot affect the output path, proven by test — *structurally: no code path exists from title to path component*
- [x] A run aborts before writing the first byte of any file that would not fit, leaving a resumable cursor
- [x] A second concurrent run against the same export root exits 3 without touching any file — *real subprocess contention*
- [x] `FloodWaitError` is slept through once and logged with its computed wake time; no tight retry; a wait beyond an explicit `--max-flood-wait` cap exits cleanly and resumably
- [x] Media that is deleted, expired, or unavailable is recorded and **skipped**, and the run continues
- [x] `--verbose` leaves the `telethon` logger at `WARNING`; `api_hash` and phone absent from captured output
- [x] Session file and every sibling are mode `0600`; export tree `0700`
- [x] `pytest` green (180 tests); `pip install -r requirements.txt -e .` from a clean venv yields a working `tg-export`

## Phases

| Phase | Name | Status |
|-------|------|--------|
| 1 | [Verification Spike](./phase-01-verification-spike.md) | Partial — offline probes settled, live probes await credentials |
| 2 | [Skeleton and Session Auth](./phase-02-skeleton-and-session-auth.md) | Complete |
| 3 | [Traversal Grouping and Filters](./phase-03-traversal-grouping-and-filters.md) | Complete |
| 4 | [Naming and Path Safety](./phase-04-naming-and-path-safety.md) | Complete |
| 5 | [Downloader State and Resume](./phase-05-downloader-state-and-resume.md) | Complete |
| 6 | [Dry-Run Estimator](./phase-06-dry-run-estimator.md) | Complete |
| 7 | [End-to-End Validation](./phase-07-end-to-end-validation.md) | Pending — requires a live group |

**Dependency chain:** 1 → 2 → 3 → 4 → 5 → 6 → 7. Phase 4 is pure and offline-testable, so it can run parallel to 3. Phase 1 gates everything.

## Open Questions

1. **Group scale** — unknown volume. Resolved by phase 6 dry-run.
2. **2FA enabled?** — resolved by phase 1 P2.
3. **Group access type** — resolved by phase 1.
4. ~~**Text-only messages in `messages.jsonl`?**~~ **Resolved (validation):** default media-bearing only, plus an `--include-text` flag that records text messages with an empty `files[]`. The flag is part of the stored filter set, since it changes what a post contains.
5. **Deleted/expiring media** — recorded as an `errors[]` entry on that message's sidecar record and counted at exit; the run continues.
6. **Stray `.part` files** — swept at startup, after the lock is acquired.
7. **Unknown-size media (`file.size is None`)** — **included** and warned, counted separately in the estimate rather than as zero. Invert if disk pressure matters more than completeness.
8. **Export dir named `g<chat_id>`, not the group title** — chosen to make path traversal structurally impossible and to survive group renames. Human name lives in `title.txt`. Reversible if you prefer readable directory names, at the cost of both properties.
9. **Should a FAILED file be permanently abandoned?** Raised by the
   post-implementation review, and genuinely unsettled by this plan. Today a
   failure advances the cursor and is recorded in `errors[]`, which is right for
   deleted or expired media but also covers a file that exhausted its three
   retries on transient network trouble — that one is never re-attempted, and
   `completed_at` is still set. The exit summary names the count. Options if this
   matters: re-drive the range with `--reset-state`, or add a `--retry-failed`
   pass that reads the sidecar. **Not changed unilaterally — this is a scope
   decision.**
10. **Concurrent exports of two groups need two `--session` files**, not just two
   `--out` directories, because the session SQLite database is locked too. The
   plan claimed per-root locking made concurrent exports work; it does, but only
   with that second flag. Documented in the README and in the exit-3 message.

## Dependencies

No cross-plan dependencies.

External: `telethon>=1.41,<2`, pinned in `requirements.txt` to the version phase 1 verified. `cryptg` is **not** installed in v1: it is a native extension sitting in Telethon's AES path, so it sees the auth key and every plaintext byte. It ships an aarch64 wheel (verified — no build step), and the reason to trust it if adopted is that the Telethon author maintains it, not that it is "CPU-side only". Don't add a supply-chain root for a throughput problem that has not been measured.

## Red Team Review

### Session — 2026-08-11

**Reviewers:** 4 (Security Adversary, Failure Mode Analyst, Assumption Destroyer, Scope & Complexity Critic)
**Findings:** 39 raw → 20 deduplicated → 15 adjudicated (13 accepted, 2 rejected, 4 corrected as overstated)
**Severity breakdown:** 7 Critical, 8 High

Evidence standard adapted for a greenfield repo: reviewers cited plan-file and research-report lines, since no source code exists to grep.

| # | Finding | Severity | Disposition | Applied To |
|---|---------|----------|-------------|------------|
| 1 | Photo `.size` documented approximate but used as an exact-equality gate that raises → poison pill | Critical | Accept | Phase 1, 5 |
| 2 | Filename index computed post-filter → breaks dedupe and determinism | Critical | Accept | Phase 3, 4, 5 |
| 3 | Download loop has no `try`; "skip deleted media" structurally impossible | Critical | Accept | Phase 5 |
| 4 | `<group-slug>` is attacker-controlled and unsanitized | Critical | Accept | Phase 4, plan |
| 5 | Flood-wait wrapper cannot protect the sweep | Critical | Accept (re-reasoned) | Phase 2, 3 |
| 6 | `--limit` advances the cursor; phase 7 idempotency step unrunnable | Critical | Accept (fix relocated) | Phase 7 |
| 7 | Phase 1 creates `.session` before any `.gitignore`; sqlite creates it `0644` before the post-hoc `chmod` | Critical | Accept | Phase 1, 2 |
| 8 | Disk guard fires ≤50 files late; "abort before first byte" unimplemented | High | Accept | Phase 5, 6 |
| 9 | Empty-`Post` contract deferred to the implementer; no completion marker | High | Accept | Phase 3, 5 |
| 10 | Dry-run ignores the cursor → `--dry-run && run` fails closed on resume | High | Accept | Phase 6 |
| 11 | No single-instance lock | High | Accept | Phase 2, 5 |
| 12 | `--reset-state` leaves the sidecar → mixed filter generations | High | Accept | Phase 5 |
| 13 | `msg.file.size is None` handled three incompatible ways | High | Accept | Phase 3, 5, 6 |
| 14 | `--verbose` unmutes telethon loggers; 2FA-env policy contradiction; gitignore coupled to default paths | High | Accept (traceback half rejected) | Phase 2 |
| 15 | Album consecutiveness across pagination unverified and untestable as guarded | High | Accept | Phase 1, 3 |

**Rejected:**
- *"Fold phase 1 into phase 2."* The ordering bug inside it is real and fixed; the merge argument adds no evidence against the gate itself, which exists to settle facts phases 3/5/6 build on. Fixed the duplication instead.
- *"Drop probe P4 (`download_media` return value) — nothing consumes it."* True as written, but finding 1's fix makes the return value the source of truth for `os.replace`.

**Corrected as overstated or factually wrong** (verified empirically during resolution):
- `cryptg` "builds from sdist on ARM64, executing at install time" — **false**. It ships a `cp312-manylinux_2_28_aarch64` wheel. The plan's *wording* was still wrong; rationale rewritten.
- `--verbose` "leaks credentials via traceback" — **false**. CPython does not print locals in tracebacks. The logger half is real and fixed.
- Session permission race rated Critical — realistically Medium on a single-user host. Fixed anyway: it is one `umask` line.
- "7 modules for 600 lines" — module count is not a defect metric; cohesion is. `estimate.py` kept. The `Sink` Protocol and the `state.py` bundling were the real findings.

**Net effect:** the resolved design is *smaller* than the reviewed one. Deleted: the `Sink` Protocol, state `counters`, the sha256 filter fingerprint, the every-50-files disk poll, the optional-preflight flag, the positional filename index, and title-based slugging.

### Whole-Plan Consistency Sweep
- Files reread: plan.md, phase-01 … phase-07
- Decision deltas checked: 15
- Reconciled stale references: filename scheme (ph3/4/5), state schema (ph5/6/7), module names `naming.py`→`paths.py` (plan/ph4/ph5), flag surface (ph2/3/5/6), dedupe predicate (ph5/7), export dir naming (plan/ph4/ph5/ph7), disk guard (plan/ph5/ph6/ph7)
- Unresolved contradictions: 0

## Implementation Log

### Session 1 — 2026-08-12/13 — phases 2–6

Phases 2–6 implemented and green: **180 tests**, all offline against synthetic
message stubs. `pip install -r requirements.txt -e .` from a clean venv yields a
working `tg-export` on Telethon **1.44.0** (aarch64 wheel).

Phase 1 could not be run — no Telegram credentials exist in this environment —
but most of what it gates was settled without them, recorded in the
[spike findings report](../reports/spike-260811-1411-telethon-api-verification-report.md):

- **P1 CONFIRMED from Telethon's source**, not from a sample: under `reverse=True`
  the iterator does `offset_id += 1` before the first request and again per page,
  and `min_id` is not consulted in the reverse branch of `_message_in_range`. So
  `offset_id` is an exclusive lower bound, the cursor stores the last handled id,
  and `iter_posts` carries a defensive `msg.id <= after_id` guard regardless.
- All 15 error classes in phase 5's taxonomy exist in 1.44.0. A missing one would
  have been an import-time crash.
- P2, P3, P5 and P6 remain live-only. The design was already answer-independent
  for all four, which is why implementation did not wait on them.

`spike/probe_telethon_api.py` is written and ready to run.

#### Module deviation

`media.py` was added, holding `media_kind` / `media_size` / `file_stem` /
`file_ext`. The plan put `media_size` in `traversal.py`, but `paths.py` needs kind
and extension for `derive_filename`, and either direction of import between those
two would have been a cycle or a second copy of the rule. One owner, two
consumers, no cycle.

One behavioral consequence worth recording: `media_kind` returns `None` when
`msg.web_preview` is set. Telethon's `Message.photo` and `.document` also return
the *web preview's* media, so classifying without that guard would have made every
message containing a URL look like an attachment — inflating counts and
downloading link-preview thumbnails.

#### Defects found and fixed during implementation

| # | Defect | Consequence had it shipped |
|---|--------|---------------------------|
| 1 | The 5-second commit throttle never flushed the tail | A run ending on a short stretch of filtered or text-only posts set `completed_at` against a stale cursor; the next run re-swept that tail to download nothing |
| 2 | `git check-ignore` reports `exports` as *not ignored* until the directory exists | The export root would be refused on the first run and accepted on every run after it. Fixed with a directory-form retry, gated to `is_dir=True` so it can never excuse a session *file* |
| 3 | `file_ext` capped extensions at 16 characters, not bytes | A multibyte suffix pushed the filename past ext4's 255-byte limit |

#### Corrections to the plan's own sketches

- **`except` ordering.** The plan checked `OSError` before the network clause, but
  `ConnectionError` *is* an `OSError`, and asyncio's `ConnectionResetError` can
  carry `errno=None` — which the errno test would have routed to "unexpected OS
  error" and aborted a healthy run. Network clause first, filesystem second.
- **Lock contention exit code.** `raise SystemExit(f"busy: ...")` exits **1**, not
  3, so it would have failed its own acceptance criterion. Raises `Abort(reason, 3)`.
- **Exit-code ownership.** `run_download` lets `Abort` and `KeyboardInterrupt`
  propagate to `cli.main()` rather than calling `sys.exit` mid-loop, and does not
  call `disconnect()` — the `connected_client` context manager owns that.
- **Bare asserts.** The ascending-sweep and album-buffer tripwires are now
  `SweepOrderError`, not `assert`: `python -O` strips assertions, and these guard
  the same silent corruption as the `AlbumSplitError` beside them.

### Red Team Review — Session 2 — 2026-08-13 (post-implementation)

Independent `code-reviewer` pass over the implementation against this plan.
**Verdict: DONE_WITH_CONCERNS.** Invariants 1 and 2 verified to hold under trace
and probe; dry-run and real-run agree exactly on posts and files for
`limit ∈ {None, 0, 1, 2}`. All eight declared deviations were assessed sound and
kept. Two reachable defects broke stated guarantees:

| # | Finding | Severity | Disposition |
|---|---------|----------|-------------|
| H1 | `--types " "` (or `,`) builds an empty kind set that drops every media kind, but `to_state()` serialized it as `None` — i.e. *no filter* | Critical | **Fixed.** Refused in `build_filters`; `to_state` made lossless as a second line of defense |
| H2 | The filename **extension** bypassed `sanitize` entirely | Critical | **Fixed.** Routed through `sanitize_ext`; plus `download_one` now records a path failure as FAILED instead of letting it escape |
| M1 | `validate` excused *any* photo size mismatch, including 0 bytes | High | **Fixed.** Zero bytes is refused for every kind |
| M2 | A FAILED file advances the cursor and is never retried | Medium | **Documented, not changed** — the plan settles this ("recorded and skipped, the run continues"). See open question 9 |
| M3 | `ServerError` (parent of `RpcCallFailError`) and the `telethon.errors.common` set escaped the taxonomy | High | **Fixed.** Both added to the retry tuple |
| M4 | `OSError` from the sidecar or state writes bypassed the exit-code contract | Medium | **Fixed.** `cli.main` maps `FATAL_ERRNO` → 3 |
| M5 | `--max-size 0` printed as `(none)`; a negative `--limit` made the two run modes disagree | Low | **Fixed.** `is not None` throughout; negative `--limit` rejected at the boundary |
| M6 | `Config.max_flood_wait` was dead | Low | **Fixed.** Removed |

H1 was the worst of these: it is silent *total* data loss, reachable from
`--types "$UNSET_VAR"`, and it defeats Invariant 3 — the very guard meant to catch
it. H2 was a permanent denial of service from one hostile filename, since the
cursor could not advance past the message that raised.

Also fixed from the review's lower-severity notes: duplicated `_fsync_dir`
(now `state.fsync_dir`, one owner), the session file being created before the git
check rather than after, dead code in `human_bytes` and `EstimateSink.add`, and a
dry-run `Mode:` line that silently measured from zero when the stored filters
differed — it now says a real run would refuse.

Two criteria the review found untested are now tested: export-tree and file modes
under the process umask, and control characters in the *extension*.

## Validation Log

### Session 1 — 2026-08-11

Verification pass skipped per the workflow guard: `## Red Team Review` above already carries evidence-backed verification, and zero `[UNVERIFIED]` tags remain across all eight plan files.

**Questions asked:** 5 (4 + 1 disambiguation)

| # | Decision point | Answer | Effect |
|---|---|---|---|
| 1 | Export directory naming | `g<chat_id>` + `title.txt` | **Confirms** the red-team fix. No change. |
| 2 | Media with no reported size | Download + warn | **Confirms** the resolved design. No change. |
| 3 | Sidecar scope | Add an `--include-text` flag | **Change** — see below |
| 4 | Disk reserve | 2 GiB | **Confirms.** No change. |
| 5 | Flood-wait ceiling | **No ceiling by default**; `--max-flood-wait N` caps it | **Change** — reverses the plan's prior default |

**Change 1 — `--include-text`.** Default remains media-bearing only. With the flag, text-only messages are recorded in the sidecar with an empty `files[]`. It joins the stored filter set because it changes what a post contains: running without it and then with it against the same cursor would otherwise silently skip the text records. Text-only messages never create a post folder and never affect `posts_with_files` counting, so the dry-run/real-run reconciliation is unaffected.

**Change 2 — flood-wait default inverted.** Previously the tool exited resumably after a wait beyond 1 hour. Now it sleeps indefinitely by default, and `--max-flood-wait N` imposes a ceiling.

The trade-off was presented and the user chose unattended completion over fail-fast: a badly throttled account now looks like a hang rather than an exit. Accepted with one mitigation — every flood wait logs at WARNING with its duration **and computed wake time**, so a long sleep is legible as a sleep. Exit code 6 is retained for the capped case.

### Whole-Plan Consistency Sweep — Validation Session 1
- Files reread: plan.md, phase-01 … phase-07
- Decision deltas checked: 2
- Reconciled: flag surface (plan/ph2/ph3/ph5), filter set membership of `--include-text` (ph3/ph5), sidecar record shape for text messages (ph5), `posts_with_files` counting (ph3/ph6), flood-wait defaults and exit code 6 (plan/ph2/ph5), README items (ph7)
- Unresolved contradictions: 0
