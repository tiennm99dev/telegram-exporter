---
phase: 5
title: "Downloader State and Resume"
status: complete
priority: P1
dependencies: [3, 4]
effort: "~4h"
---

# Phase 5: Downloader State and Resume

## Overview

The download path: fetch media sequentially, write durably, dedupe, persist the resume cursor and the `messages.jsonl` sidecar. This is where correctness under interruption is won or lost.

Governing principle: **the filesystem is the state; the state file is a hint; the sidecar is the log.** Everything below follows from it.

## Requirements

- Functional: download each `Post`'s media into its folder; skip completed files; resume after any interruption; record every file and every failure
- Non-functional: concurrency 1; no partial file ever presented as complete; the cursor never points past an incomplete post

## Architecture

### Durability and dedupe are one decision, not two

Telethon has no byte-level resume — `download_media` writes from offset 0 every call. So resumability lives at *message* granularity, and completeness is proven by **arrival at the target path**:

```
per file:  write -> flush -> fsync(fd) -> os.replace(tmp, target)
per post:  fsync(post_dir) -> sidecar.append + fsync -> state.commit (tmp -> fsync -> replace -> fsync(root))
```

With `fsync` before `os.replace`, a file at the target path can only have arrived fully downloaded. Therefore:

```python
if target.exists():
    if target.stat().st_size > 0:
        return SKIPPED
    target.unlink()            # zero-byte junk from ENOSPC; re-fetch
```

**Existence, not size equality.** Size equality would only add detection of *external* corruption — a fabricated threat for a personal archive — and it is precisely what created the poison pill: phase 6 documents photo `.size` as the size *variant* Telethon selects, i.e. approximate, while an exact-equality gate would delete every completed photo on resume and then raise. The fsync is what licenses the simpler predicate.

Stray `.part` files are swept at startup, **after** the lock is acquired — the sweep is the destructive operation, so locking after it locks after the damage.

### Validator: advisory, never raises

```python
actual, declared = tmp.stat().st_size, media_size(msg)
if declared is None or actual == declared:   accept
elif kind(msg) == "photo":                   accept, log DEBUG      # variant size, expected
else:                                        RETRY; on exhaustion -> FAILED
```

Documents, video, and audio carry an exact `document.size`, so a mismatch there is real truncation. Photos are the only approximate case. This design is correct **whether or not** phase 1 P5 finds photo sizes exact.

### Error taxonomy — the failure paths get the same rigor as the happy path

Three buckets. `FloodWaitError` never appears here: it is owned by `with_flood_retry`, because retries during an active wait are counted as fresh violations.

```python
TRANSIENT_ERRNO = {EAGAIN, EINTR, EIO, ETIMEDOUT, ECONNRESET}
FATAL_ERRNO     = {ENOSPC, EDQUOT, EROFS, EACCES}
MAX_ATTEMPTS, BACKOFF = 3, lambda n: 5 * 2 ** (n - 1)      # 5, 10, 20 s

async def download_one(client, entity, post_dir, msg) -> Result:
    target = safe_join(post_dir, derive_filename(msg))
    if target.exists():
        if target.stat().st_size > 0: return SKIPPED
        target.unlink()
    check_disk(msg)                                        # before the first byte
    tmp, refreshed = target.with_name(target.name + ".part"), False

    for attempt in range(1, MAX_ATTEMPTS + 1):
        try:
            with open(tmp, "wb") as f:
                await with_flood_retry(lambda: client.download_media(msg, file=f))
                f.flush(); os.fsync(f.fileno())
            if validate(tmp, msg) is RETRY:
                tmp.unlink(missing_ok=True); await sleep(BACKOFF(attempt)); continue
            os.replace(tmp, target)
            return DOWNLOADED(target)

        # ---- SKIP: this message is unavailable; the run continues ----
        except FileReferenceExpiredError:
            tmp.unlink(missing_ok=True)
            if refreshed: return FAILED(msg, "file reference expired")
            msg = await client.get_messages(entity, ids=msg.id); refreshed = True
            if msg is None or msg.media is None: return FAILED(msg, "message deleted")
            continue
        except (MediaEmptyError, FileIdInvalidError, FilerefUpgradeNeededError):
            tmp.unlink(missing_ok=True); return FAILED(msg, "media unavailable or expired")

        # ---- ABORT: the run cannot continue ----
        except (AuthKeyUnregisteredError, AuthKeyDuplicatedError,
                SessionRevokedError, UserDeactivatedBanError):
            tmp.unlink(missing_ok=True); raise Abort("session invalidated — re-login", 4)
        except (ChannelPrivateError, ChatForbiddenError, ChannelInvalidError):
            tmp.unlink(missing_ok=True); raise Abort("lost access to the group", 5)

        except OSError as e:                                # fatal checked FIRST
            tmp.unlink(missing_ok=True)
            if e.errno in FATAL_ERRNO:     raise Abort(f"filesystem: {e}", 3)
            if e.errno in TRANSIENT_ERRNO: await sleep(BACKOFF(attempt)); continue
            raise Abort(f"unexpected OS error: {e}", 1)

        except (ConnectionError, asyncio.TimeoutError, asyncio.IncompleteReadError,
                rpcerrorlist.TimedOutError, rpcerrorlist.RpcCallFailError):
            tmp.unlink(missing_ok=True); await sleep(BACKOFF(attempt)); continue

    return FAILED(msg, f"exhausted {MAX_ATTEMPTS} attempts")
```

`FileReferenceExpiredError` handling is **not optional** — file references expire in hours, so a 20-hour run will hit it. One `get_messages` refresh is four lines and the highest-value item in this table.

If phase 1 P4 finds `download_media` rejects a file object, fall back to download-to-path then re-open and fsync before rename.

### The sink loop — Invariant 2 by construction

```python
async def run_download(posts, state, sidecar, root):
    try:
        async for post in posts:
            media, results = [m for m in post.messages if m.media], []   # results must be bound
            if media:                                          # text-only members download nothing
                results = [await download_one(...) for msg in media]
                fsync_dir(post_dir)
            if post.messages:                                  # may be text-only under --include-text
                sidecar.append(post, results); sidecar.fsync()
            if wrote_files or (monotonic() - last_commit) > 5.0:
                state.commit(post.max_message_id)          # only after a COMPLETE post
        state.mark_completed()
    except Abort as a:
        log.error(a.reason); sys.exit(a.code)              # cursor already correct
    except KeyboardInterrupt:
        log.info("interrupted — re-run the same command to resume"); sys.exit(130)
    finally:
        await client.disconnect()
```

**The cursor is never written from an abort path**, so it cannot point past an in-flight post. "Commit the cursor and exit" is deleted as a concept — it was the mechanism by which the disk guard could have violated Invariant 2.

The 5-second throttle stops 100k filtered messages from causing 100k state writes. Losing an uncommitted sweep-only advance costs a cheap re-sweep with zero downloads.

Exit codes: `0` ok · `1` unexpected · `2` dry-run won't fit · `3` disk or lock contention · `4` auth · `5` access · `6` flood ceiling (**only reachable when `--max-flood-wait` is explicitly set** — unset means sleep indefinitely) · `7` filter/chat mismatch · `130` interrupt.

### Disk guard — one mechanism, per file

```python
def check_disk(msg):
    need = (media_size(msg) or 0) + cfg.min_free          # default 2 GiB
    if shutil.disk_usage(out_dir).free < need:
        raise Abort(f"insufficient disk: need {need}, have {free}", 3)
```

Replaces all three previous mechanisms. `shutil.disk_usage` is one `statvfs` — microseconds, unmeasurable against multi-second downloads at concurrency 1. The every-50-files poll optimized a cost that does not exist while opening a ~100 GB blind spot: a single Telegram file reaches 2–4 GB, so up to ~100 GB could be written between two polls on a 41 GB-free host. Comparing against *this file's* size closes that structurally. The 2 GiB reserve is principled — it covers one entire unknown-size file at the non-premium upload cap.

### State file

```json
{
  "version": 1,
  "chat_id": -1001234567890,
  "chat_title": "My Group",
  "filters": {"types": ["photo","video"], "since": "2026-01-01T00:00:00Z",
              "until": null, "max_size": 104857600, "include_text": false},
  "cursor_id": 1042,
  "completed_at": null,
  "started_at": "2026-08-11T07:00:00Z",
  "updated_at": "2026-08-11T07:42:11Z"
}
```

- **One cursor field.** `swept_to_id` vs `last_done_id` is a distinction without a difference here: the sweep is a generator driven by the sink at concurrency 1, so it can never run ahead. Two fields would invent a divergence to manage. `cursor_id` means: *every post with `max_message_id <= cursor_id` has been handled under `filters`*.
- **Filters stored verbatim; the sha256 fingerprint is deleted.** A digest is one-way, and two phases require the tool to *name* what changed. Comparing dicts does that for free, with less code than hashing.
- **`counters` deleted.** Derivable from the sidecar and double-counting on every resume (re-processed albums re-increment), which would make phase 7 chase phantom discrepancies. Each run prints its own in-memory session totals at exit.
- **`completed_at`** — set on clean exhaustion, cleared to `null` at every run start. The only way to answer "did my 20-hour export finish?" without guessing.
- Mismatch on `filters` or `chat_id` → refuse, print the diff, exit 7:
  ```
  filter mismatch:
    types:    ['photo','video'] -> ['video']
    max_size: 100MB -> (none)
  re-run with the original filters, or --reset-state to start over
  ```

`--limit` is **not** in the filter set — it changes when we stop, not what a post contains, so including it would make a limited test run poison every later full run. A `--limit 20` run is a resumable partial export whose cursor genuinely reflects completed work.

### Sidecar

```json
{"message_id":1043,"post_id":1042,"grouped_id":88,"date":"2026-07-04T12:00:00Z",
 "sender_id":777,"sender_name":"Alice","caption":"beach trip","reply_to":1001,
 "files":[{"path":"1042/1043_photo.jpg","size":1234567,"declared_size":1234999,
           "name":"photo.jpg","mime":"image/jpeg","kind":"photo"}],
 "errors":[]}
```

Under `--include-text` a text-only message produces the same record shape with `"files":[]` — no folder is created for it. Consumers distinguish media from context by `files` being empty, not by a separate record type.

<!-- Updated: Validation Session 1 - --include-text record shape; include_text joins the stored filter set -->

`size` is bytes on disk; `declared_size` appears only when it differs — which incidentally accumulates empirical data on photo-variant accuracy. Paths are relative to the export root. `sender_name` comes from `msg.sender` **only if already cached** — never an extra `get_entity` call, which on a 100k-message sweep would be 100k extra RPCs and a flood ban.

**A record is an append-only event: "the files written for this message in this run."** The correct consumer rule is therefore **union over all records for a `message_id`**, not last-write-wins. Last-write-wins was actively wrong — after a filter change it would report a narrower file list than what is on disk.

Startup repair: scan back from EOF for the last `\n`; if the trailing segment is non-empty and not valid JSON, truncate to that offset and warn. A host crash mid-`write()` otherwise leaves a partial line that breaks every JSONL consumer.

`--reset-state` **rotates** `messages.jsonl` → `messages-<utc-ts>.jsonl` and starts fresh, so the current sidecar always describes exactly one filter regime. It does **not** delete downloaded files — dedupe reclaims them; deleting 30 GB to change a filter is hostile.

## Related Code Files

- Create: `src/telegram_exporter/downloader.py`, `state.py`, `sidecar.py`
- Create: `tests/test_download_decisions.py`, `tests/test_state.py`
- Modify: `src/telegram_exporter/cli.py` (wire the download path, `--reset-state`, `--min-free`, `--max-flood-wait`)

## Implementation Steps

1. `state.py`: load/create, filter and chat_id comparison with a named diff, atomic save (tmp → fsync → replace → fsync dir), `commit`, `mark_completed`
2. `sidecar.py`: append-only JSONL writer, relative paths, fsync, startup partial-line repair, rotation on `--reset-state`
3. `downloader.py`: `check_disk`, `validate`, `download_one` with the full taxonomy, startup `.part` sweep (after lock acquisition)
4. `downloader.py`: `run_download` per the sketch — sidecar before cursor, cursor unreachable from abort paths
5. Progress output: per-file line with post id, filename, size, running total; session totals at exit including `N files failed (see messages.jsonl)`
6. Write `<root>/title.txt` once on first run
7. Tests per the table below

## Test Table — `test_download_decisions.py` (tmpdir + fake downloader, no network)

| Case | Expected |
|---|---|
| target absent | DOWNLOADED |
| target exists, non-empty | SKIPPED, no download call |
| target exists, zero bytes | unlinked, re-downloaded |
| stray `.part` at startup | swept |
| `.part` present and target valid | target wins, `.part` removed |
| `media_size` is `None` | stated policy, asserted |
| `file.name == "../../../etc/passwd"` | lands inside `post_dir` |
| free space < file size + reserve | `Abort(3)` before any write |

## Success Criteria

- [ ] Full run downloads every media file into album-aware folders
- [ ] `kill -9` mid-run then re-run: no re-downloads, no gaps, no orphan `.part`
- [ ] Interrupting mid-album re-downloads that album and leaves no duplicates
- [ ] Every row of the test table passes
- [ ] Re-running with changed filters refuses and prints a per-filter diff (exit 7)
- [ ] A different `chat_id` against an existing state file refuses (exit 7)
- [ ] `messages.jsonl` has one line per media message with correct relative paths; a truncated final line is repaired at startup
- [ ] `--reset-state` rotates the sidecar and preserves downloaded files
- [ ] Media with a dead file reference is refreshed once, then recorded in `errors[]` and **skipped** — the run continues past it
- [ ] With `--include-text`, text-only messages appear in the sidecar with `"files":[]` and create no folder
- [ ] Toggling `--include-text` against an existing cursor triggers the filter-mismatch refusal (exit 7)
- [ ] A flood wait logs its duration **and computed wake time**; with no `--max-flood-wait` set, the run sleeps rather than exiting
- [ ] Simulated low disk aborts before writing, with a resumable cursor
- [ ] `completed_at` is set only on clean exhaustion
- [ ] `test_no_concurrency.py` (AST scan) finds no `gather`/`create_task`/`to_thread`/thread pool in `src/`

## Risk Assessment

- **Cursor ahead of reality** → permanent silent gaps; the worst failure here because it looks like success. Invariant 2 is structural: `state.commit` is unreachable from every abort path.
- **Right-sized corrupt file after host crash** → fsync before `os.replace`, plus dir fsync before the cursor claims the post durable. This is what makes existence-based dedupe sound.
- **Poison-pill validator** → validator never raises; photos excused; design independent of phase 1 P5's answer.
- **Unhandled exception kills an unattended 20-hour run** → full taxonomy above; transient errors retry with backoff, terminal ones exit cleanly with a correct cursor.
- **Filter/cursor confusion** → verbatim filters, named diff, hard refusal.
- **Disk exhaustion mid-write** → per-file pre-check against this file's size plus a 2 GiB reserve.
- **Concurrent runs** → flock acquired before the `.part` sweep (phase 2).
- **Sidecar misread after a filter change** → union semantics, documented; `--reset-state` rotates.
- **Temptation to parallelize when it feels slow** → flood-wait escalation to multi-hour lockouts. Enforced by an AST test, not by discipline.
