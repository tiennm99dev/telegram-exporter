# Research Report: Telegram Group Media Exporter (Telethon / Python)

**Conducted:** 2026-08-11 14:11 (Asia/Saigon)
**Target env:** Linux ARM64 (aarch64), headless, Python 3.12.3, ffmpeg 6.1.1, 41 GB free disk
**Stack (confirmed by user, not re-evaluated):** Telethon + Python, album-aware folder layout
**Scope in v1:** resume+dedupe, `messages.jsonl` sidecar, type/date/size filters, dry-run size estimate

---

## Table of Contents

1. [Executive Summary](#executive-summary)
2. [Methodology](#methodology)
3. [Findings](#findings)
   - [1. Auth & Session Persistence](#1-auth--session-persistence)
   - [2. History Traversal & Resume](#2-history-traversal--resume)
   - [3. Album Grouping via grouped_id](#3-album-grouping-via-grouped_id)
   - [4. Media Download & Filenames](#4-media-download--filenames)
   - [5. Flood Waits & Concurrency](#5-flood-waits--concurrency)
   - [6. Dry-Run Size Estimation](#6-dry-run-size-estimation)
   - [7. Packaging & CLI](#7-packaging--cli)
4. [Proposed Architecture](#proposed-architecture)
5. [Common Pitfalls](#common-pitfalls)
6. [Confidence Ledger](#confidence-ledger)
7. [Next Steps](#next-steps)
8. [Unresolved Questions](#unresolved-questions)
9. [References](#references)

---

## Executive Summary

Three findings drive the whole design.

**Concurrency is the wrong instinct.** Telethon does not parallelize file downloads internally, and the docs/issues are explicit that parallel downloads only make `FloodWaitError` arrive *sooner*. Worse, retrying during an active flood wait is itself counted as a violation — a 7s wait can escalate past 4 hours within an hour of retry-looping. **Default concurrency must be 1**, with explicit flood-wait sleeping. This kills any worker-pool design before it is written.

**Telethon has no byte-level download resume.** `download_media` writes from offset 0 every call. Resumability must therefore be built at *message granularity*: download to a `.part` file, `os.replace()` atomically on completion, and treat any surviving `.part` as incomplete garbage to be re-fetched. Combined with an oldest→newest sweep (`reverse=True`) and a persisted last-completed message id, this gives a monotonic, crash-safe cursor.

**Albums need buffering, not lookups.** An album is N separate messages sharing a `grouped_id`; there is no "fetch the album" API. The common StackOverflow pattern (scan ±10 message ids around a post) is for the single-post case and is wasteful here. Since we sweep every message anyway, group by buffering consecutive same-`grouped_id` messages and flushing when the id changes.

---

## Methodology

- Sources consulted: 5 research passes (1 doc fetch, 4 web searches) → ~20 distinct sources
- Gemini CLI: **absent on this host**, fell back to WebSearch per skill config
- Primary authority: `docs.telethon.dev` (1.44.0 stable) + LonamiWebs/Telethon GitHub issues
- Key terms: `grouped_id album`, `FloodWaitError bulk download`, `StringSession vs SQLiteSession`, `iter_messages reverse offset_id`, `pyproject.toml CLI 2026`
- **Limitation:** `docs.telethon.dev/en/stable/modules/client.html` is a single very large page; the fetch truncated before the full `iter_messages` and `download_media` signatures. Items depending on it are flagged in the [Confidence Ledger](#confidence-ledger).

---

## Findings

### 1. Auth & Session Persistence

Bot API is structurally disqualified (no history access; 20 MB download cap) — MTProto user account is the only path. `api_id`/`api_hash` from my.telegram.org.

| | SQLiteSession (default) | StringSession |
|---|---|---|
| Storage | on-disk `.session` SQLite DB | in-memory, serializable to base64 string |
| Best for | long-lived local tool | serverless / env-var-injected secrets |
| Gotcha | **must call `disconnect()` before exit** or state is not flushed | must persist the string yourself |

**Recommendation: SQLiteSession file**, `chmod 600`, path configurable, `.gitignore`d. This is a localhost tool with a persistent filesystem — StringSession solves a containerization problem we do not have (YAGNI). One-time interactive login (phone + code), every later run unattended.

Security, verbatim from the docs: *"Never give the saved session file to anyone, since they would gain instant access to all your messages and contacts."* Treat `.session` as a credential — never commit, never log its path contents.

**2FA is unresolved by the sources.** The searched docs did not cover the 2FA password flow. Telethon's `client.start(phone=..., password=...)` and `SessionPasswordNeededError` are the expected mechanism — **verify at implementation time**, and never accept the 2FA password as a CLI argument (it lands in shell history); prompt interactively or read from env.

### 2. History Traversal & Resume

`iter_messages(entity, ...)` with parameters `limit`, `offset_id`, `min_id`, `max_id`, `offset_date`, `filter`, `reverse` — all confirmed present, full semantics truncated in the fetch.

**Design: sweep oldest→newest with `reverse=True`.** Rationale: message ids increase monotonically over time, so "highest fully-completed id" is a single-integer resume cursor that is always correct, even if the run is killed mid-album. A newest-first sweep would need an interval set instead.

```python
# resume: continue after the last fully-completed message
async for msg in client.iter_messages(entity, reverse=True, offset_id=state.last_done_id):
    ...
```

**Footgun to verify:** `offset_id` semantics *invert* with `reverse`. Newest-first, it is an upper bound (start below this id); with `reverse=True` it acts as a lower bound (start above this id). Confirm empirically with a 3-message probe before trusting the resume path — an off-by-one here silently re-downloads or silently skips.

Pagination is handled internally (~100 messages/request); no manual paging needed.

### 3. Album Grouping via `grouped_id`

Confirmed: *"Albums are a way to send multiple photos or videos as separate messages with the same grouped identifier. It is possible to know that it's a grouped message thanks to `grouped_id`."*

Album members arrive as **consecutive** messages in a chronological sweep. Grouping algorithm:

```
buffer = []
for msg in sweep(reverse=True):
    if msg.grouped_id is None:
        flush(buffer); emit_single(msg)
    elif buffer and msg.grouped_id != buffer[0].grouped_id:
        flush(buffer); buffer = [msg]
    else:
        buffer.append(msg)
flush(buffer)   # end of history
```

**Folder id = the lowest message id in the group** (first member, since we sweep ascending). Deterministic and resume-stable: a re-run recomputes the same folder name.

**Resume interacts with albums.** Only advance `last_done_id` once *every* member of a group has landed — otherwise a crash mid-album leaves a half-folder that resume skips past. Simplest correct rule: commit the cursor at group-flush boundaries, not per file.

Do **not** use the "iterate ±N ids around the post" pattern from issue #3788 — that solves the single-known-post case, and duplicates work in a full sweep.

### 4. Media Download & Filenames

Confirmed chunked download primitive:

```python
async download_file(
    input_location: hints.FileLike,
    file: hints.OutFileLike = None,
    *,
    part_size_kb: float = None,
    file_size: int = None,
    progress_callback: hints.ProgressCallback = None,
    dc_id: int = None,
    key: bytes = None,
    iv: bytes = None
) -> bytes | None
```

For our purposes the higher-level `download_media(message, file=path, progress_callback=...)` is correct — it wraps chunking and DC routing. `part_size_kb` tuning is a micro-optimization; skip it (YAGNI).

**Filename/extension derivation:** `message.file` exposes `.name`, `.ext`, `.mime_type`, `.size`. Preference order:
1. `DocumentAttributeFilename` via `message.file.name` — real filename for documents
2. `message.file.ext` + mime type — for photos/voice/round video, which carry no filename
3. Fallback `bin`

**Sanitize aggressively.** Filenames are attacker-controlled strings from arbitrary group members: strip `/`, `..`, NUL, leading dots, control chars, and cap length. Always join under the target dir and assert the resolved path stays inside it. A crafted `../../../.ssh/authorized_keys` is a real, cheap attack on a naive exporter.

**No byte-level resume** (see Executive Summary). Pattern:

```
tmp = folder / f"{idx}_{name}.part"
download_media(msg, file=tmp)
os.replace(tmp, folder / f"{idx}_{name}")     # atomic on same filesystem
```

**Dedupe check:** skip when the final file exists *and* its size equals `message.file.size`. Size-match is cheap and catches truncation; hashing every file is wasted I/O at multi-GB scale.

### 5. Flood Waits & Concurrency

The single most decision-relevant finding.

- `client.flood_sleep_threshold` (default **60s**): Telethon auto-sleeps on flood waits ≤ threshold, raises `FloodWaitError` above it.
- *"The library does not download or upload files in parallel"* — no internal parallelism to inherit.
- *"The limiting factor in the long run are FloodWaitError, and using parallel download or uploads only makes them occur sooner."*
- **Escalation is real:** *"a wait that starts at 7 seconds can escalate past four hours within less than an hour of repeated restarts, because every retry during an active FloodWait is itself treated as another violation."*

**Design rules:**
1. Sequential downloads. Default concurrency **1**. Do not ship a `--workers` flag in v1 — it is a footgun that trades a small speedup for hour-long lockouts.
2. Raise `flood_sleep_threshold` (e.g. 120s) so routine small waits are absorbed silently.
3. Catch `FloodWaitError` explicitly, `await asyncio.sleep(e.seconds + jitter)`, then continue. **Never** tight-retry, never restart the process to "get around" a wait.
4. Log every flood wait with its duration — it is the primary signal for whether the run is healthy.

### 6. Dry-Run Size Estimation

`message.file.size` gives bytes without fetching content. Dry-run = full `iter_messages` sweep with zero `download_media` calls: cheap (~100 messages/request, no file-DC traffic), and the flood-wait risk is low because history reads are far cheaper than file reads.

Output: total media count, total bytes, breakdown by type, and **free-disk delta**. With 41 GB free on `/config`, an export that does not fit should fail loudly *before* the first byte, not at 97%.

Caveat: for photos, `.size` reflects the size variant Telethon selects (largest by default); treat photo totals as a close estimate, document sizes as exact.

### 7. Packaging & CLI

Consensus from the packaging guides: `argparse` for simple tools with no dependencies, Click for complex multi-command CLIs, Typer for type-hint ergonomics (built on Click).

**Recommendation: `argparse`.** One command, ~8 flags, no subcommands. Adding Typer means +2 transitive deps to save perhaps 25 lines. YAGNI/KISS. If subcommands ever appear, Typer is a contained swap.

Packaging: `pyproject.toml` with `[project.scripts]` entry point (`tg-export = "telegram_exporter.cli:main"`) — the current standard, declarative and static.

**`uv` is not installed on this host.** Options: `pip install -e .` inside a venv (works today, zero setup), or install `uv` first. The venv path is fine; nothing here needs uv.

---

## Proposed Architecture

```
                     ┌──────────────┐
                     │  cli.py      │  argparse: --group --out --dry-run
                     │              │  --since/--until --types --max-size
                     └──────┬───────┘
                            │
                     ┌──────▼───────┐
                     │  client.py   │  auth, SQLiteSession, flood-wait wrapper
                     └──────┬───────┘
                            │
        ┌───────────────────▼────────────────────┐
        │  sweep.py                              │
        │  iter_messages(reverse=True,           │
        │                offset_id=cursor)       │
        └───────────────────┬────────────────────┘
                            │ messages, ascending
        ┌───────────────────▼────────────────────┐
        │  grouper.py   buffer by grouped_id     │
        └───────────────────┬────────────────────┘
                            │ logical posts
              ┌─────────────┴─────────────┐
              │                           │
     ┌────────▼────────┐         ┌────────▼────────┐
     │ estimator.py    │         │ downloader.py   │
     │ sum .file.size  │         │ .part → rename  │
     │ (dry-run only)  │         │ size-match skip │
     └─────────────────┘         └────────┬────────┘
                                          │
                            ┌─────────────▼─────────────┐
                            │ state.py   cursor + jsonl │
                            │ commit at group boundary  │
                            └───────────────────────────┘
```

Output layout (album-aware, per user decision):

```
exports/<group>/
  1042/                  <- album, grouped_id shared, folder = first msg id
    0_photo.jpg
    1_photo.jpg
    2_video.mp4
  1047/                  <- standalone message
    0_document.pdf
  messages.jsonl         <- id, date, sender, caption, grouped_id, reply_to, files[]
  .export-state.json     <- last_done_id, counters
```

---

## Common Pitfalls

| Pitfall | Consequence | Mitigation |
|---|---|---|
| Parallel downloads to "go faster" | Flood waits arrive sooner, escalate to hours | Sequential, concurrency 1 |
| Retrying during an active flood wait | 7s wait → 4h+ lockout | Sleep the full `e.seconds`, never restart to bypass |
| Forgetting `disconnect()` | SQLite session not flushed, re-login next run | `try/finally` or async context manager |
| Unsanitized `DocumentAttributeFilename` | Path traversal outside the export dir | Sanitize + assert resolved path is inside target |
| Advancing the resume cursor per file | Crash mid-album → permanently half-downloaded folder | Commit cursor only at group flush |
| Assuming `download_media` resumes | Silent truncated files | `.part` + atomic rename; treat stray `.part` as garbage |
| `offset_id` semantics under `reverse=True` | Silent skip or full re-download | Verify empirically with a 3-message probe |
| Committing the `.session` file | Full account takeover | `.gitignore`, `chmod 600` |
| No pre-flight size check | Fills 41 GB disk mid-run | Dry-run + free-space assertion |

---

## Confidence Ledger

Honest separation of what the sources confirmed from what needs checking in code.

**Verified by sources:**
- `flood_sleep_threshold` default 60s, auto-sleep ≤ threshold
- Telethon does not parallelize file transfers; parallelism accelerates flood waits
- Flood-wait escalation from retries during an active wait
- `download_file` full signature incl. `part_size_kb`, `progress_callback`, `dc_id`
- `grouped_id` identifies album membership; albums are separate messages
- SQLiteSession vs StringSession trade-offs; `disconnect()` flush requirement
- `iter_messages` accepts `reverse`, `offset_id`, `min_id`, `max_id`, `offset_date`, `filter`, `limit`
- `[project.scripts]` in `pyproject.toml` as the entry-point standard

**Not confirmed — verify during implementation:**
- Exact `offset_id` direction under `reverse=True` (**highest-risk item**; breaks resume silently)
- 2FA / `SessionPasswordNeededError` flow — sources did not cover it
- `message.file` attribute surface (`.name`/`.ext`/`.size`/`.mime_type`) — strongly indicated by the custom-types docs, not read verbatim
- Whether `download_media` returns the final path string in all media branches
- Photo `.size` accuracy vs the actually-downloaded variant

---

## Next Steps

1. **Spike first, build second** (~30 min): a throwaway script that logs in, iterates 20 messages of a real group, prints `id / grouped_id / file.name / file.size`, and downloads one album. Settles all four unverified items above at once.
2. Run `/ck:plan` to phase the implementation against the architecture sketch.
3. Phase order: auth+session → sweep+resume → grouper → downloader → jsonl sidecar → filters → dry-run estimator.
4. Add `.session`, `exports/`, `.export-state.json` to `.gitignore` **before** the first login.

---

## Unresolved Questions

1. **Group scale?** Unknown message/media count. If the group is >40 GB the 41 GB free disk is the binding constraint and dry-run becomes mandatory before any real run.
2. **2FA enabled on the account?** Changes the login flow and whether an interactive prompt is required on first run.
3. **Group access type** — public/private, and is the account already a member? Private groups need the account joined; `get_entity` resolution differs for public username vs invite-link groups.
4. **Should non-media messages appear in `messages.jsonl`?** Text-only messages give context to captions and replies, but inflate the sidecar. Default assumption: media-bearing messages only.
5. **Deleted/expiring media and service messages** — skip silently or record as gaps in the sidecar?
6. **Retention of `.part` files** on a failed run — auto-clean at startup, or leave for inspection?

---

## References

### Official Documentation
- [Client class — Telethon 1.44.0](https://docs.telethon.dev/en/stable/modules/client.html) — `download_file` signature, `iter_messages` params
- [RPC Errors — Telethon 1.44.0](https://docs.telethon.dev/en/stable/concepts/errors.html) — FloodWaitError semantics
- [FAQ — Telethon 1.44.0](https://docs.telethon.dev/en/stable/quick-references/faq.html) — no-parallel-transfer statement
- [Session Files — Telethon 1.44.0](https://docs.telethon.dev/en/stable/concepts/sessions.html) — SQLite vs String sessions
- [Sessions module — Telethon 1.44.0](https://docs.telethon.dev/en/stable/modules/sessions.html)
- [Custom package (types) — Telethon 1.44.0](https://docs.telethon.dev/en/stable/modules/custom.html) — `Message.file` wrapper
- [Telethon 1.44.0 full PDF](https://media.readthedocs.org/pdf/telethon/stable/telethon.pdf) — offline reference for the truncated pages
- [Creating and packaging command-line tools — PyPA](https://packaging.python.org/en/latest/guides/creating-command-line-tools/)
- [Writing your pyproject.toml — PyPA](https://packaging.python.org/en/latest/guides/writing-pyproject-toml/)

### Issues & Community
- [Download all media in a post by grouped_id — Telethon #3788](https://github.com/LonamiWebs/Telethon/issues/3788)
- [Detect a grouped message (album) — Telethon #1216](https://github.com/LonamiWebs/Telethon/issues/1216)
- [FloodWaitError in client.download_media — Telethon #1426](https://github.com/LonamiWebs/Telethon/issues/1426)
- [FloodWaitError during document download — Telethon #206](https://github.com/LonamiWebs/Telethon/issues/206)
- [Converting StringSession to SQLiteSession — Telethon #3660](https://github.com/LonamiWebs/Telethon/issues/3660)
- [Telethon — DeepWiki](https://deepwiki.com/LonamiWebs/Telethon)

### Further Reading
- [Fix Telegram FloodWait Error Fast](https://membertel.com/blog/how-to-fix-telegram-floodwait-error-fast/) — flood-wait escalation behavior
- [telebackup — multi-connection Telegram downloader](https://github.com/xwc9527/telebackup) — prior art for parallel/checkpoint downloading; **contrary to our sequential decision**, review only if throughput becomes a proven problem
- [Packaging Python CLI apps the modern way with uv](https://thisdavej.com/packaging-python-command-line-apps-the-modern-way-with-uv/)
- [Building Python CLI tools: Click, Typer, argparse](https://inventivehq.com/blog/python-cli-tools-guide)

### Glossary
- **MTProto** — Telegram's native protocol; full user-account API access (vs the restricted Bot API)
- **DC** — Telegram data center; large files may live on a DC other than the session's home DC
- **`grouped_id`** — shared identifier marking messages that belong to one album
- **FloodWait** — server-imposed cooldown after too-rapid requests; escalates on retry
