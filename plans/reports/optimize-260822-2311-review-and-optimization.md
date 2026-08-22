# Review + optimization pass — telegram-exporter

Two agents ran in parallel (full-codebase review, suite verification); findings
cross-checked against source before acting. Suite: **204 → 214 passing**,
coverage held at **93%**. Every fix below has a test that fails without it
(verified by stashing the fix and re-running).

Companion reports: `code-review-260822-2259-full-codebase-audit.md`,
`tester-260822-2259-suite-verification.md`.

## Baseline

Suite was not runnable — no pytest in the environment. Working setup, now in the
README:

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt -e ".[dev]"
.venv/bin/python -m pytest
```

All 7 safety-critical domains already had real tests (resume, partial files, path
sanitization, album grouping, flood waits, pagination, locking). The correctness
core survived adversarial reading: sanitizer, `safe_join`, album grouping, cursor
ordering, error taxonomy, no unbounded message accumulation, `_sender_name` doing
zero RPC.

## Fixed — correctness

**Session lock taken after the resource it guards** (`cli.py`). The lock sat
inside `connected_client`, so `connect()` + `_login()` had already written the
shared SQLite session. Two runs on the default session each passed their own
per-root lock, both opened the same database, and the loser exited 3 *after*
causing the corruption it reported. Lock now wraps `connected_client` in `_run`;
`assert_not_committable` runs first so a refused path leaves no lock file.
`--dry-run` is inside the lock too — it opens the same session file. README's
"dry-run can always run alongside an export" narrowed to "with its own
`--session`", per your call.

**Completion guard disabled by one skipped file** (`downloader.py`).
`if totals.failed and not (downloaded or skipped)` required that *nothing* had
succeeded, so any resume across a partly-complete export could fail every
remaining file and still set `completed_at` at end-of-history — a broken export
reading as finished. Now refuses when `failed > downloaded + skipped`. A
legitimate all-present tail outnumbers its own stray failures and still
completes.

**Renewed file reference never fetched** (`downloader.py`). Expiry on the final
attempt consumed the last slot, so the refreshed reference was discarded and the
message reported "exhausted 3 attempts" having been tried twice. The retry loop
no longer counts a refresh as an attempt; the `refreshed` latch still bounds it.
Docstring says a 20-hour run *will* reach this arm.

**Sidecar repair could eat 1 MiB of valid records** (`sidecar.py`). A partial
record larger than the scan window contains no newline, so `rfind` returned -1
and the file was truncated to `end - window` — mid-record, still unreadable, one
window shorter. Window now grows until a newline is found.

**`--reset-state` ordering + rotate collision** (`cli.py`, `sidecar.py`). The
zeroed cursor became durable before the old sidecar rotated, leaving exactly the
mixed-generation state rotation exists to prevent; rotation now precedes
`State.open` (safe only on this path, which has no compatibility check to fail).
Two resets in one second silently clobbered the first archive via `os.replace` —
now serialized, and the rename is `fsync`'d like every other rename here.

**`title.txt` written raw and non-atomically** (`downloader.py`, `paths.py`). The
one untrusted string reaching disk unfiltered: `cat title.txt` fed ANSI escapes
and U+202E overrides to the operator. Now stripped of the same categories
`paths.sanitize` strips (new `strip_unprintable`, shared set so the two rules
can't drift), and written tmp → fsync → replace → fsync(dir).

## Fixed — cost

**Per-post fsync on already-complete posts** (`downloader.py`). The post
directory was fsynced every post, including posts where every file was already
present and nothing was renamed. A resume across a mostly-complete export paid
one fsync per post to re-durably-record a directory entry an earlier run already
had. Now gated on an actual download.

**Triple `stat` per already-present file** (`downloader.py`). `exists()` +
`stat()` + a third inside `_result`, on the single most common path of any
resume. One `stat` now, reused as the recorded size.

Note: moving fsync off the loop entirely is **not** available —
`tests/test_no_concurrency.py` forbids `to_thread`/`run_in_executor` by AST scan,
a deliberate decision (flood-wait escalation). Fewer fsyncs is the only lever.

## Not changed — deferred by decision

**Sweep starts at message id 1 under `--since`.** Verified in the pinned wheel
(`_MessagesIter._init`): `reverse=True` + no `offset_date` → `offset_id = 1`.
`--since` on a month of a 1M-message group burns ~9,900 discarded round trips.
`--until` never breaks early. Both carry invariant risk — `offset_date` can start
mid-album and break filter-invariant post identity; an `--until` break assumes
date/id monotonicity, false for imported history. Recorded as step **12b** in
phase 7: measure the swept/kept ratio first, decide with numbers.

## Not changed — reported only

- `resolve_entity` catches `ValueError` only; a TypeError or RPC error skips the
  whole fallback chain → exit 1 + traceback instead of the documented 5.
  `_find_in_dialogs` is the only network loop not behind a flood primitive.
  Largest untested reachable surface in `src/` (monkeypatched away in tests).
- `SystemExit` in `session.py` is a `BaseException` — bypasses every `cli.main`
  handler and lands on interpreter exit 1, routing around the `Abort` contract.
- `_login` has no `PhoneCodeInvalidError` handling: a typo on first login exits 1
  with a traceback instead of re-prompting.
- `completed_at` is cleared at `State.open` before any work, so an interrupted
  re-run of a finished export reads incomplete forever.
- `_nearest_existing_dir` ≡ `_measurable_dir`, differing only in fallback.
- Dead `raise AssertionError` in `human_bytes` (loop always returns at TiB).
- `exclusive` can print holder `?` — races the holder's truncate/write.
- `Filters._warned` is a mutable log latch on a frozen, hashable, serialized
  value object. Works; surprising.

## Docs

README: dry-run/session-lock semantics corrected in both places; `flock`-on-NFS
caveat added; exit code 2 documented honestly (argparse exits 2, colliding with
dry-run SHORT — table previously promised 1); venv creation added to setup.

## Unresolved questions

1. `resolve_entity` hardening — widen the `except` and route
   `_find_in_dialogs` through a flood primitive? Needs a call on whether dialog
   scanning should retry at all.
2. `SystemExit` in `session.py` → convert to `Abort` for one exit-code contract?
3. `completed_at` cleared eagerly — keep (a re-run is genuinely incomplete) or
   only clear once work begins?
