---
phase: 2
title: "Skeleton and Session Auth"
status: complete
priority: P1
dependencies: [1]
effort: "~2h"
---

# Phase 2: Skeleton and Session Auth

## Overview

Package skeleton, the authenticated Telethon client, the flood-wait primitives every network call routes through, and the process-level safety rails: umask, logging policy, git-safety assertion, and the single-instance lock.

## Requirements

- Functional: one-time interactive login; later runs non-interactive; `tg-export --help` after `pip install -e .`
- Non-functional: no file is ever group/world-readable at any instant; no credential in argv, env-where-forbidden, or logs; two concurrent runs cannot corrupt each other

## Architecture

### Process rails — `cli.main()`, before anything else

```python
os.umask(0o077)   # FIRST statement. Covers .session and every sqlite sibling,
                  # export tree, messages.jsonl, .part, state, lock — one line, no per-file chmod.
```

Verified empirically: `sqlite3.connect()` on a fresh path yields `0644` under a default umask, and the auth key is written *during* login — so a post-hoc `chmod` is provably too late. With `umask(0o077)` set first, the db and its `-journal` are both `0600`.

### Session path

```python
SUFFIX = ".session"

def prepare_session_path(p: Path) -> Path:
    # Telethon appends '.session' when absent — chmod-ing the un-suffixed path protects nothing
    p = p if p.name.endswith(SUFFIX) else p.with_name(p.name + SUFFIX)
    p.parent.mkdir(parents=True, exist_ok=True)
    try:
        os.close(os.open(p, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600))
    except FileExistsError:
        os.chmod(p, 0o600)
    return p

def assert_session_private(p: Path) -> None:          # this is the test, not a checkbox
    for f in p.parent.glob(p.name + "*"):
        if stat.S_IMODE(f.stat().st_mode) & 0o077:
            raise SystemExit(f"insecure mode on {f}: {oct(f.stat().st_mode)}")
```

Default path `${XDG_STATE_HOME:-~/.local/state}/tg-export/<name>.session` — **outside the repo**, so the safe location is the default and the git check only bites deliberate choices.

### Git safety — robust to `--out` / `--session` moving the targets

Adding `.gitignore` patterns cannot help when the operator can point a flag anywhere. Assert at runtime:

```python
def assert_not_committable(p: Path) -> None:
    """No-op outside a git work tree. rc 0=ignored, 1=NOT ignored, 128=not a repo."""
    try:
        rc = subprocess.run(["git", "check-ignore", "-q", str(p)],
                            cwd=p.parent, capture_output=True).returncode
    except FileNotFoundError:
        return
    if rc == 1:
        raise SystemExit(f"refusing: {p} is inside a git repo and is not gitignored.\n"
                         f"add it to .gitignore, or choose a path outside the repo.")
```

Called on the session path and export root at startup. Exit codes verified empirically, including on nonexistent paths.

### Single-instance lock

```python
@contextmanager
def exclusive(path: Path):
    """flock releases on process death, kill -9 included — no stale-lock cleanup to get wrong."""
    path.parent.mkdir(parents=True, exist_ok=True)
    f = open(path, "a+")
    try:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        f.seek(0)
        raise SystemExit(f"busy: another tg-export holds {path} [{f.read().strip() or '?'}]")
    f.seek(0); f.truncate()
    f.write(f"pid={os.getpid()} host={socket.gethostname()} started={now_iso()}\n"); f.flush()
    try:
        yield
    finally:
        f.close()
```

Two call sites: `<export_root>/.export.lock` (covers `.part` files, state, sidecar) and `<session>.lock` (two Telethon clients on one SQLite session is what Telethon itself warns about). Per-root, not global — exporting two different groups concurrently is legitimate.

Contention: fail fast, exit 3, no wait, no `--force`. The message names pid/host/start-time, so "did it hang?" gets an answer instead of a second process. **Dry-run takes no lock** — it writes nothing.

### Logging policy

```python
def configure_logging(verbose: bool) -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    logging.getLogger("telegram_exporter").setLevel(logging.DEBUG if verbose else logging.INFO)
    logging.getLogger("telethon").setLevel(logging.WARNING)   # unconditional floor
    logging.getLogger("asyncio").setLevel(logging.WARNING)
```

Never touch the root level. `--verbose` means *our* logger only — telethon's request logging carries `api_id`, `phone_number`, and `phone_code_hash`. No `--debug-telethon` flag.

Note: CPython does not print local variables in tracebacks, so no exception-scrubbing machinery is needed. Just don't install `rich` or `better-exceptions`.

### Credential policy — one table, referenced by every other doc

| Credential | Source | Why |
|---|---|---|
| `api_id`, `api_hash` | env `TG_API_ID` / `TG_API_HASH` only | needed every run; revocable at my.telegram.org |
| phone | prompt; `TG_PHONE` optional | PII, not a secret |
| login code | **prompt only** | single-use, 5-minute TTL |
| 2FA password | **`getpass` only** — no env, no file, no flag | once per session lifetime; prompting costs nothing |

`.env` **may** contain `TG_API_ID`, `TG_API_HASH`, `TG_PHONE`. It **must not** contain the 2FA password, login code, or session data. The app does **not** read `.env` — no `python-dotenv` to read a file the shell already handles (`set -a; . ./.env; set +a`).

### Flood-wait primitives — two, not one

The single-value wrapper cannot protect the sweep: `iter_messages` is consumed with `async for`, and Telethon raises `FloodWaitError` on the *next page fetch*, deep inside the loop — so it escapes a wrapper that only guards construction, and re-invoking the factory would restart the sweep from `after_id`.

```python
def _flood_sleep(e, max_wait_s, where=""):
    """max_wait_s is None by default -> sleep however long Telegram demands.
    Always log the WAKE TIME, not just the duration: an unattended 4-hour sleep must
    read as a sleep, not as a hang. This is the mitigation for the no-ceiling default."""
    if max_wait_s is not None and e.seconds > max_wait_s:
        raise Abort(f"flood wait {e.seconds}s exceeds --max-flood-wait {max_wait_s}s", 6)
    wake = datetime.now(UTC) + timedelta(seconds=e.seconds)
    log.warning("flood wait %ss%s — sleeping until %s", e.seconds, where, wake.isoformat())
    return asyncio.sleep(e.seconds + random.uniform(1.0, 3.0))

async def with_flood_retry(make_coro, *, max_wait_s: int | None = None):
    """Download path. Never tight-retry — retries during an active wait are fresh violations."""
    while True:
        try:
            return await make_coro()
        except FloodWaitError as e:
            await _flood_sleep(e, max_wait_s)

async def aiter_with_flood_retry(make_agen, *, start_id, max_wait_s: int | None = None):
    """Sweep path. Catches mid-iteration waits and rebuilds the generator from the
    last id actually yielded, so no message is skipped or replayed."""
    last = start_id
    while True:
        try:
            async for item in make_agen(last):
                last = item.id
                yield item
            return
        except FloodWaitError as e:
            await _flood_sleep(e, max_wait_s, where=f" mid-sweep after id {last}")
```

`max_wait_s=None` is the default per the validation decision: unattended completion is preferred over failing fast. The wake-time log is what keeps that choice diagnosable.

Also set `client.flood_sleep_threshold = 120` so routine short waits are absorbed inside Telethon.

## Related Code Files

- Create: `pyproject.toml`, `README.md`
- Create: `src/telegram_exporter/{__init__,cli,session}.py`
- Create: `tests/{__init__,test_logging,test_lock}.py`
- Modify: `.gitignore` (verify coverage; phase 1 created it)

## Implementation Steps

1. `pyproject.toml` — `telethon>=1.41,<2`, `[project.scripts] tg-export = "telegram_exporter.cli:main"`, hatchling backend, `dev = ["pytest>=8"]`. Install with `pip install -r requirements.txt -e .` so the phase-1-verified version is what's used.
2. **Verify** (do not recreate) that `.gitignore` covers the production session path and export root.
3. `cli.py`: `os.umask(0o077)` as the first statement, then `configure_logging`, argparse, `assert_not_committable`, locks, dispatch.
4. `session.py`: credential loading with an actionable error pointing at my.telegram.org; `prepare_session_path`; client construction; login (phone/code prompts, 2FA via `getpass`); `assert_session_private` after connect; async context manager guaranteeing `disconnect()` — SQLite state is not flushed without it.
5. `session.py`: both flood primitives and the `Abort` exception type.
6. Entity resolution for `--group`: numeric id, `@username`, `t.me` link. Use `telethon.utils.get_peer_id(entity)` as the canonical int for the export root. Clear message when the account is not a member.
7. Full flag surface (later phases wire their own):
   ```
   tg-export --group <id|@username|link> --out DIR
             [--dry-run] [--since YYYY-MM-DD] [--until YYYY-MM-DD]
             [--types photo,video,document,audio,voice] [--max-size 100MB]
             [--include-text] [--limit N] [--session PATH] [--reset-state]
             [--min-free 2GiB] [--max-flood-wait SECONDS] [--verbose]
   ```
   `--max-flood-wait` is **unset by default — the tool sleeps through any flood wait, however long** (validation decision). Passing a number imposes a ceiling above which the run exits cleanly with a resumable cursor (exit 6).

   <!-- Updated: Validation Session 1 - added --include-text; --max-flood-wait default inverted to no ceiling -->

8. Tests: `test_logging.py` (telethon logger stays ≥ WARNING even with `verbose=True`), `test_lock.py` (subprocess holds the lock; second acquire exits 3).

## Success Criteria

- [ ] `pip install -r requirements.txt -e ".[dev]"` succeeds on Python 3.12 / aarch64
- [ ] `tg-export --help` prints the full flag surface
- [ ] Production session path does not exist before first login (asserted) — phase 1's spike session is separate
- [ ] After login: session and every sibling matching `session*` are mode `0600`
- [ ] Second run is non-interactive: `tg-export --group G --limit 0 </dev/null` exits 0 (EOF makes any prompt fail loudly)
- [ ] `test_logging.py`: `configure_logging(verbose=True)` leaves `telethon` at ≥ `WARNING`
- [ ] `test_lock.py`: concurrent acquire exits 3 without touching any file
- [ ] `assert_not_committable` refuses a session or export path that is inside the repo and not ignored
- [ ] `disconnect()` runs on both success and exception paths
- [ ] 2FA password is reachable only via `getpass` — no flag, no env read, anywhere in the source

## Risk Assessment

- **Session file group/world-readable at any instant** → `umask(0o077)` first, `O_EXCL` pre-create, post-connect assertion. Verified: chmod-after-connect is too late *and* targets the wrong filename when `--session foo` is passed.
- **Session committed** → gitignore from phase 1, default path outside the repo, runtime `git check-ignore` assert.
- **Credentials in logs** → telethon logger pinned at WARNING unconditionally; enforced by test, not by checkbox.
- **Two policies for the 2FA password** → one table here; phase 1 amended to match.
- **Concurrent runs corrupting state** → flock on export root and session, acquired *before* the destructive `.part` sweep.
- **Missing `disconnect()`** → context manager, not discipline.
- **Flood wait during the sweep** → `aiter_with_flood_retry`; the single-value wrapper alone cannot cover `async for`.
