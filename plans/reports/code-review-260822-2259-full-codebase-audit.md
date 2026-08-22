# Full-codebase review — telegram-exporter

Date: 2026-08-22 · Scope: read-only, no source/test/config modified
Reviewed: `src/telegram_exporter/*.py` (~1,900 LOC), `README.md`, plan index, `tests/` (skim)
Method: line-by-line read; Telethon 1.44.0 wheel downloaded to a scratch dir and
`telethon/client/messages.py` read to check the traversal claims. Test suite NOT run
(pytest unavailable here; execution owned by another agent).

## Verdict

The design is unusually disciplined and the three invariants hold as written. The
correctness core — sanitizer, `safe_join`, album grouping, cursor ordering, error
taxonomy — survived a deliberate attempt to break it. What is left is a small set
of ordering/edge defects at the *outside* of that core (process startup, sidecar
rotation, sidecar repair, entity resolution) plus two genuinely large throughput
wins that the current sweep leaves on the table.

Nothing here contradicts an already-adjudicated decision. Where a finding touches
plan open question 9, 11 or 12, that is stated and the finding is narrowed to the
part those questions do not cover.

---

## Must fix (correctness)

### M1 — the session lock is acquired after the thing it protects (High)

`cli.py:195` takes `exclusive(session_lock)` inside `async with connected_client(...)`
(`cli.py:157`). By the time the lock is tested, `connected_client` has already run
`prepare_session_path`, `TelegramClient(...)`, `await client.connect()` and `_login()`
(`session.py:246-254`) against the same SQLite file the lock exists to guard.

Failure scenario (the exact one the README documents as supported, `README.md:242-245`):
two groups, two `--out` dirs, default `--session`. Both processes pass the per-root
lock (different roots), both open and write the same `default.session` SQLite DB and
both register as clients on the same auth key. Only *then* does B hit the session
lock and exit 3. Observed damage window covers Telethon's session writes; the
plausible outcomes are `sqlite3.OperationalError: database is locked` escaping as a
generic exit 1, or the auth-key duplication the README itself calls a corruption
hazard. The exit-3 message ("one SQLite session cannot serve two clients") is
printed *after* the violation it describes.

Fix: acquire the session lock in `_run` before `connected_client` is entered, i.e.
hoist it above line 157. The root lock can stay where it is (it guards the tree, and
the `.part` sweep is still the first destructive act under it).

Related, same root cause: `_dry_run` (`cli.py:175`) takes **no** session lock, and
`README.md:247-248` promises "`--dry-run` takes no lock at all, so it can always be
run against an export that is currently in progress." With the default session that
promise puts a second Telethon client on the in-progress run's session file — which
`README.md:244` calls a corruption hazard. The two README paragraphs contradict each
other. Either the dry run must take the session lock (and the README sentence must
gain "…with its own `--session`"), or the sentence must be narrowed. This is the one
finding where the fix depends on product intent, so pick before coding.

No test covers the session lock at all: `tests/test_lock.py` drives `exclusive()`
directly and `test_different_export_roots_do_not_contend` only proves the *root*
lock is per-root.

### M2 — `Sidecar.repair()` can discard 1 MiB of valid records (Medium)

`sidecar.py:58-77`. `cut = tail.rfind(b"\n")` returns `-1` when the last 1 MiB window
contains no newline; that case is not distinguished. `trailing` then becomes the
whole window, `json.loads` fails, and `f.truncate(end - len(trailing))` cuts 1 MiB
off the file — landing mid-line, so the file is *still* unterminated and the repair
did net damage.

Reachable when the sidecar's tail is one long unterminated stretch: an interrupted
write on a filesystem that padded the tail, or any future record shape larger than
the window. The `window == end` sub-case (small file, no newline anywhere) truncates
to 0, which is correct; only the `end > window` case is wrong.

Fix: treat `cut == -1` as "no record boundary in the window" and either widen the
window or refuse and tell the operator, rather than truncating blind.

Untested: `tests/test_sidecar.py` covers partial-line, complete-line-no-newline,
intact and empty — never the no-newline-in-window case.

### M3 — `--reset-state` can silently clobber the previous sidecar generation, and resets state before rotating (Medium)

`sidecar.py:79-88` + `cli.py:201-205`.

1. `rotate()` builds `messages-<%Y%m%dT%H%M%SZ>.jsonl` at one-second granularity and
   uses `os.replace`, which overwrites without complaint. Two `--reset-state` runs in
   the same wall-clock second (scripted loop, or a fast filter sweep) destroy the
   first archive. `rotate()`'s own docstring says the old file "stays inspectable".
2. Ordering: `State.open(..., reset=True)` already wrote the zeroed cursor
   (`state.py:95`) *before* `sidecar.rotate()` runs. If rotation fails (EACCES,
   read-only dir, ENOSPC), cli exits via the `OSError` handler with the cursor reset
   and the old sidecar still in place — exactly the mixed-filter-generation state
   rotation exists to prevent.
3. `rotate()` does not `fsync_dir` the rename, unlike every other durability-critical
   rename in the codebase (`state.py:133`).

Fix: rotate first, then `State.open(reset=True)`; pick a non-colliding target name
(suffix a counter when the stamp exists); fsync the directory.

### M4 — `resolve_entity` has no tests and two escape paths (Medium)

`session.py:260-299`.

- `except ValueError as e` at line 275 wraps both `int(target)` and
  `client.get_entity(int(target))`. Any non-`ValueError` from that call — a
  `TypeError`, or an RPC error such as `ChannelInvalidError`/`ChannelPrivateError` for
  a cached-but-lost id — bypasses the username/dialog fallback entirely and lands on
  `cli.main`'s generic `except Exception` as exit 1 with a traceback, instead of the
  documented exit 5 with the "confirm this account is a member" guidance.
- `_find_in_dialogs` (`session.py:296`) iterates **every** dialog with no flood-wait
  wrapper. It is the only network loop in the codebase not routed through
  `with_flood_retry`/`aiter_with_flood_retry`, and `session.py:1-2` claims every
  network call routes through the two primitives. A `FloodWaitError` on a
  many-thousand-dialog account exits 1 instead of sleeping — before a single byte of
  a 20-hour export.

Verification gap: no test in the suite references `resolve_entity` or
`_find_in_dialogs`; `tests/test_cli_dispatch.py:88` monkeypatches the whole function
away. This is the largest untested reachable surface in `src/`.

### M5 — argparse failures exit 2, colliding with the documented dry-run verdict (Low, but it breaks a published contract)

`cli.py:227` calls `parse_args()` outside the `try`. Any argparse rejection —
including `_non_negative`'s `ArgumentTypeError` for `--limit -1` (`cli.py:98-105`) and
a non-integer `--limit` — exits **2** (verified against CPython argparse). `README.md:255`
says a bad argument is exit 1, and `README.md:256` reserves 2 for "`--dry-run` says it
will not fit". A wrapper script cannot distinguish "you typed the flag wrong" from
"buy a bigger disk".

Fix: `parser.exit_on_error = False` / catch `argparse.ArgumentError`, or subclass and
override `error()` to exit 1. Untested either way.

### M6 — `completed_at` is cleared on disk before any work is done (Low)

`state.py:82` + `state.py:95`: `State.open` sets `completed_at = None` and immediately
`save()`s. If the run then ends without exhausting the sweep — Ctrl-C (130), exit 6 on
a flood ceiling, exit 3 on disk, an `Abort` from `download_one` — a previously
*completed* export is now permanently recorded as incomplete, and `state.py:116-117`
calls that field "the only way to answer 'did my 20-hour export finish?'".

Fix: defer the clear until the first `commit()` actually moves the cursor, or keep a
`last_completed_at` alongside.

### M7 — the "everything failed" guard is defeated by a single SKIPPED file (High; narrows plan open question 11)

`downloader.py:448`: `if totals.failed and not (totals.downloaded or totals.skipped)`.

Open question 11 already owns "no circuit breaker; a dead session walks the whole
history". This is a different, narrower hole in the H-D fix that shipped: `skipped`
in that condition means the guard only fires on a run where *nothing at all*
succeeded. Any resumed run that re-processes one post — guaranteed after a hard kill
mid-post, and after any `--reset-state` — produces at least one SKIPPED result. From
that point on, a media DC that fails every remaining file for the rest of the history
still gets `mark_completed()` (`downloader.py:458`) and a cursor at end-of-history.
The operator's export reads as finished over a tree that is missing most of it, and
`README.md:15-16`'s "re-run and it resumes" does nothing because the cursor is at the
end.

`tests/test_download_decisions.py:457` proves the guard for the fresh-run case only;
no test injects a SKIPPED result alongside universal failure.

Options (a threshold judgement, so not picked here):
- drop `skipped` from the condition — but then a legitimate resume whose tail is all
  SKIPPED plus a couple of genuinely-deleted media stops being marked complete;
- gate on a ratio (`failed > downloaded`) or on consecutive failures, which is
  open question 11's circuit breaker arriving anyway.

### M8 — a refreshed file reference can be thrown away unused (Low)

`downloader.py:308-319`. The `FileReferenceExpiredError` arm consumes a retry slot.
If expiry lands on attempt 3 of 3, `continue` exits the loop and the message is
reported as `exhausted 3 attempts` — the freshly refreshed reference is never
actually used. The docstring at line 309 says a 20-hour run *will* hit this arm, so
the tail case is not hypothetical.

Fix: don't count the refresh as an attempt (e.g. `for attempt in ...` over a small
budget that the refresh path extends by one), or retry immediately after refresh
before falling through.

---

## Worth doing (optimization — a long-running bulk exporter)

### O1 — `--since` still sweeps the history from message id 1 (largest win)

`traversal.py:193-194` always builds `client.iter_messages(entity, reverse=True,
offset_id=since)` and never passes `offset_date`. Verified against the pinned
Telethon 1.44.0 (`telethon/client/messages.py` `_MessagesIter._init`, lines 52-58):
under `reverse=True`, when `offset_id` is falsy and `offset_date` is unset it forces
`offset_id = 1` — i.e. the beginning of history — and the comment on line 56 states
"offset_id has priority over offset_date". So on a first run with `--since`, every
message before the cutoff is fetched over the wire and dropped locally by
`keep()` (`traversal.py:160-163`).

Cost: one `messages.getHistory` per 100 messages. `--since` covering the last month
of a 1M-message group is ~9,900 round trips of pure waste — hours of wall clock and
a materially higher chance of the flood-wait escalation the whole design is built to
avoid.

Caveat that must be handled, not ignored: entering the sweep at a date boundary can
land *inside* an album, and `_close` (`traversal.py:237`) would then take `post_id`
from the wrong member — breaking Invariant 1. Under the current
`offset_id`-from-cursor scheme that is unreachable (the cursor is always a
`max_message_id`, never mid-album), so `offset_date` introduces the risk. Any
implementation needs to either snap back to the album's first member or accept and
document the boundary post. Recommend measuring the win on the real group first
(phase 7) before taking on that complexity.

### O2 — `--until` never terminates the sweep

Same call site. Once `msg.date > filters.until`, `keep()` drops every remaining
message (`traversal.py:163`) but `iter_posts` keeps paging to end-of-history, yielding
empty Posts so the cursor advances. Same order-of-magnitude waste as O1, for zero
downloaded bytes.

An early break is cheap (flush `buf`, `return`) and is *safer* than O1 — the cursor
simply stops at the last in-range post, and re-running with the same `--until`
re-sweeps one page and stops.

Caveat: it assumes message date is monotonic in message id. That is true for normal
posting but not guaranteed for imported history (`messages.importChatHistory` /
"import from WhatsApp" produce old dates on new ids). Given this project's explicit
no-silent-gaps stance, gate the break on a small tolerance or verify the group has no
imported range before shipping it. Flagging rather than recommending unconditionally.

### O3 — the cursor commit could be time-gated unconditionally

`downloader.py:432-435`: `wrote` forces a `state.save()` on every post that
downloaded or failed a file. Each `save()` is a full JSON re-serialize plus two
fsyncs (`state.py:127-133`), and `run_download` adds `fsync_dir(target_dir)`
(`downloader.py:420`) and `sidecar.fsync()` (`downloader.py:426`) — roughly 4 fsyncs
per post.

The `wrote` clause is not load-bearing for correctness: a cursor that lags is always
safe (Invariant 2 forbids it pointing *ahead*), and the only cost of lagging is
re-processing a few posts whose files are then existence-deduped for free
(`downloader.py:283-285`). Gating all commits on `COMMIT_INTERVAL_S` would cut state
writes by ~2 fsyncs/post at no correctness cost. Meaningful on a group of many small
files or on network/rotational storage; negligible on NVMe with large media, so
measure before bothering.

Related and *not* actionable: all of this fsync/statvfs/rglob work is synchronous on
the event loop, which serializes it against Telethon's receive loop and keepalive.
The obvious remedy (`asyncio.to_thread`, `run_in_executor`) is explicitly forbidden by
`tests/test_no_concurrency.py:20-24`, and that guard encodes a locked plan decision.
So the actionable lever is "do fewer fsyncs", not "move them off the loop". I checked
whether the stall can itself drop the connection: `telethon/client/updates.py:516-544`
sends keepalive pings fire-and-forget with no response deadline, so a multi-second
fsync stall degrades throughput but does not by itself force a reconnect.

### O4 — `sweep_part_files` walks the entire export tree at every startup

`downloader.py:257`: `root.rglob(f"*{PART_SUFFIX}")`. On a mature export (hundreds of
thousands of files across as many post dirs) this is a full recursive walk before the
first byte, seconds-to-minutes on a cold cache or a network filesystem, on every run
including no-op resumes. A `.part` can legitimately be anywhere, so it cannot simply
be scoped — but it can be skipped when the previous run recorded a clean exit
(`completed_at`, or a new "exited cleanly" flag), since a clean exit leaves no `.part`
behind by construction.

### O5 — cheap redundancies

- `downloader.py:284` stats the target, then `_result` (`downloader.py:378`) stats it
  again. Two `stat()` per already-present file; on a `--reset-state` re-drive of a
  100k-file export that is 100k avoidable syscalls.
- `estimate.py` reads the state file three times for one dry run: `resume_from`
  (line 111), `_mode_line` → `stored_filters_differ` (line 94). Also two independent
  copies of the same `data.get("filters") != filters.to_state()` comparison
  (lines 95 and 114), with a third semantic copy in `state._assert_compatible`
  (line 161).

---

## Optional (simplification / DRY / hardening)

- **Duplicated helper.** `session._nearest_existing_dir` (line 96) and
  `estimate._measurable_dir` (line 66) are the same walk-up-to-an-existing-dir
  function with different fallbacks (`None` vs `Path.cwd()`). One owner, two callers.
- **Dead code.** `downloader.py:173` `raise AssertionError("unreachable: ...")` is
  genuinely unreachable — the loop returns at `"TiB"` because of the `or unit == "TiB"`
  guard on line 170. The plan's session-2 log claims dead code in `human_bytes` was
  already removed; this line is what remains.
- **`title.txt` is the one untrusted string written unsanitized.** `write_title`
  (`downloader.py:475-479`) writes the server-supplied, admin-editable group title
  raw. `paths.py:30-42` goes to real trouble to strip `Cc`/`Cf` (ANSI escapes, U+202E
  RLO) out of *filenames* for exactly this threat, and `cli.py:163` logs the title
  through `%r` so the log is safe — but `cat title.txt` feeds those bytes straight to
  the operator's terminal. `paths.sanitize` already exists; reusing it here (or at
  least stripping `_STRIPPED_CATEGORIES`) closes the last instance of the class.
  Also `write_text` is a truncate-then-write, so a crash leaves an empty or partial
  `title.txt` — the only non-atomic write in a codebase that is otherwise scrupulous
  about it.
- **`Filters._warned`** (`traversal.py:80`) is a mutable log latch bolted onto an
  otherwise pure, frozen, hashable value object that gets serialized into the state
  file. It works (the set is excluded from `compare`, so hashing and `to_state` are
  unaffected), but it means a `Filters` instance is single-use and couples a logging
  concern into the stored filter identity. A module-level latch in `traversal` would
  be less surprising.
- **`exclusive` contention message can print `?`.** `cli.py:80-81`: the contender
  reads the lock file while the holder may be between `truncate()` and `write()`
  (`cli.py:89-90`), yielding the `"?"` holder. Cosmetic, but it degrades the one
  message whose whole purpose is to answer "who is holding this?".
- **flock on network filesystems.** `README.md:238-240` presents the lock as
  unconditional. `fcntl.flock` semantics on NFS depend on the mount and server; a
  bulk media exporter pointed at a NAS is a plausible deployment. Worth one sentence
  in the README rather than a code change.
- **`_real_run` mutates the export tree before the state validation.** `cli.py:196-197`
  runs `sweep_part_files` and `write_title` before `State.open` can refuse with exit 7.
  `cli.py:199` claims "State first … before the sidecar is touched", which is true of
  the sidecar only. Harmless in practice (`.part` files are always disposable, the
  title is idempotent), but the comment overstates it.
- **`closed: set[int]`** (`traversal.py:190`) accumulates one `grouped_id` for the
  whole history. Bounded and small (tens of MB at a million messages), correct as is —
  noting it only because it is the codebase's one monotonically growing structure.

---

## Checked and confirmed correct — do not re-flag

- **`offset_id` as an exclusive lower bound with `reverse=True`.** Verified against
  the pinned wheel: `_MessagesIter._init` lines 52-58 do `offset_id += 1`,
  `_update_offset` (line 262-265) does it again per page, and `_message_in_range`
  (line 249-251) never consults `min_id` in the reverse branch. `traversal.py:198`'s
  belt-and-braces `msg.id <= after_id` guard is genuinely redundant, and worth keeping.
- **Flood-wait regeneration across a mid-album pause.** `aiter_with_flood_retry`
  (`session.py:181-200`) rebuilds from the last *yielded* id while `iter_posts` keeps
  `buf` alive across the pause. No member is skipped or replayed, and `prev` cannot
  false-positive after the restart.
- **No exception can be thrown *into* the sweep generator by the consumer.** An
  exception in `run_download`'s loop body closes the generator (GeneratorExit) rather
  than surfacing at the `yield item` inside `except FloodWaitError`, so the retry arm
  cannot swallow an `Abort` and silently restart the sweep. I specifically went
  looking for this.
- **No unbounded message accumulation.** `iter_posts` is a true generator; `buf` is
  capped at `MAX_ALBUM_BUFFER = 20` (`traversal.py:214-217`); neither run mode
  materializes a message list. The "holds all messages in a list" failure mode is
  absent.
- **Filename collisions are structurally impossible** inside a post dir, even though
  `_truncate_utf8` (`paths.py:152`) is lossy: the `{msg.id}_` prefix
  (`paths.py:103`) is unique per message and one message carries at most one media.
- **The sanitizer has no hole I could find.** `..`, `...`, `/`, `foo/`, `C:/x/y`,
  `a/../b`, `./..` as an extension, and a NUL-bearing extension all reduce to a leaf
  or to the fallback. Worst-case component length is ~229 bytes against ext4's 255.
- **Partial-post recovery is consistent.** When `download_one` raises `Abort`
  mid-album, files already `os.replace`d are on disk with no sidecar record and no
  cursor advance; the resumed run re-processes the post, reports them SKIPPED, and
  writes the record. Nothing is lost or double-counted.
- **`_sender_name` performs no RPC.** `sidecar.py:141` reads the `Message.sender`
  property, which returns the cache populated by `_finish_init`
  (`telethon/client/messages.py:205`) — not `get_sender()`. The N+1 the docstring
  warns about is genuinely avoided.
- **`media_kind`'s `web_preview` guard is correctly ordered** (`media.py:31`): a
  message carrying `MessageMediaWebPage` is excluded before `.photo`/`.document` can
  misclassify it.
- **A cursor can never land mid-album** under the current `offset_id` scheme, because
  every committed value is a `max_message_id` of a closed group. (This stops being
  true if O1 is implemented — see that finding.)
- **`assert_not_committable`'s trailing-slash retry** is gated to `is_dir=True`
  (`session.py:136`) and cannot excuse a session *file*, exactly as the comment claims.
- **`sanitize_ext` is an identity transform for ordinary extensions**, so no
  previously downloaded file is orphaned — re-confirmed, since dedupe depends on it.

Two lower-confidence notes, called out so they are not mistaken for clean bills:
`assert_session_private` and `assert_not_committable` raise `SystemExit`
(`session.py:93`, `session.py:144`), a `BaseException` that bypasses every handler in
`cli.main` and reaches the interpreter — the message prints and the process exits 1,
which happens to be defensible but is the only place in the codebase that routes
around the `Abort`/exit-code contract. And `_login`'s `sign_in` retry
(`session.py:231-233`) has no handling for a wrong code (`PhoneCodeInvalidError`),
so a typo on first login exits 1 with a traceback rather than a re-prompt.

---

## Verification gaps (behaviour with no test that plausibly regresses)

Ranked by reachability × blast radius:

1. `session.resolve_entity` and `_find_in_dialogs` — zero tests; monkeypatched away in
   the one end-to-end file. Covers M4.
2. The session lock's existence and placement — `tests/test_lock.py` never constructs
   it. Covers M1.
3. `mark_completed` with SKIPPED-plus-universal-failure — covers M7.
4. `Sidecar.repair` with no newline in the tail window — covers M2.
5. argparse rejection exit codes (`--limit -1`, `--limit x`) — covers M5.
6. `Sidecar.rotate` colliding on the same-second stamp — covers M3.
7. `write_title` with a control-character/RLO title, and `title.txt` after a
   truncate-then-crash.
8. `completed_at` surviving an interrupted re-run of a finished export — covers M6.
9. `FileReferenceExpiredError` arriving on the final attempt — covers M8.
10. `FloodWaitError` raised from `Fetcher.refresh` (the `with_flood_retry` wrapper on
    `get_messages` is unexercised).

---

## Plan status

Phases 2-6 read as complete and consistent with the code; phase 1 and phase 7 remain
correctly marked pending on live credentials. Open questions 9, 11 and 12 are still
genuinely open and are the right place for the FAILED-retry, circuit-breaker and
0-byte-document decisions — M7 is a *refinement* of 11's scope, not a replacement.
Recommend the lead add M1 (session-lock ordering, with the `--dry-run` README
contradiction) as a blocking item before phase 7, since phase 7 is the first time two
real clients could touch one session file.

## Unresolved questions

1. `--dry-run` concurrent with a real run: should it take the session lock (safe, but
   breaks the README's "always") or should the README be narrowed to "with its own
   `--session`"? Product call.
2. M7 threshold: drop `skipped` from the guard, or move straight to open question 11's
   consecutive-failure circuit breaker? Both change when a run is called complete.
3. O1/O2: is a server-side date offset worth the mid-album-entry risk to Invariant 1,
   and is date/id monotonicity safe to assume for the target group (any imported
   history)? Both are better answered with phase 7's real numbers.
4. Is `title.txt` intended to be terminal-safe? It is the only untrusted string
   written raw, and the answer decides whether `write_title` reuses `paths.sanitize`.
