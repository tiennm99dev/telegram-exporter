"""Credentials, session file preparation, the authenticated client, and the two
flood-wait primitives every network call routes through.

Credential policy (the one authoritative copy):

    api_id, api_hash | env TG_API_ID / TG_API_HASH only | needed every run
    phone            | prompt; TG_PHONE optional        | PII, not a secret
    login code       | prompt only                      | single-use, 5 min TTL
    2FA password     | getpass only - no env, no file, no flag

Nothing here reads a .env file. The shell already does that
(`set -a; . ./.env; set +a`).
"""

from __future__ import annotations

import asyncio
import logging
import os
import random
import stat
import subprocess
from contextlib import asynccontextmanager
from datetime import UTC, datetime, timedelta
from getpass import getpass
from pathlib import Path
from typing import AsyncIterator, Callable

from telethon import TelegramClient, utils
from telethon.errors import FloodWaitError, SessionPasswordNeededError

log = logging.getLogger("telegram_exporter.session")

SUFFIX = ".session"

# Routine short waits are absorbed inside Telethon rather than bubbling up.
FLOOD_SLEEP_THRESHOLD = 120


class Abort(Exception):
    """A run-ending condition that carries the process exit code.

    Raised instead of calling sys.exit deep in the call stack, so the caller
    decides when to exit and the resume cursor is never written from an error
    path.
    """

    def __init__(self, reason: str, code: int) -> None:
        super().__init__(reason)
        self.reason = reason
        self.code = code


# --------------------------------------------------------------------------- #
# Session path
# --------------------------------------------------------------------------- #

def default_session_path() -> Path:
    """Outside the repo by default, so the safe location needs no opt-in and the
    git check only bites a deliberately chosen path."""
    base = os.environ.get("XDG_STATE_HOME") or str(Path.home() / ".local" / "state")
    return Path(base) / "tg-export" / f"default{SUFFIX}"


def with_session_suffix(p: Path) -> Path:
    """Telethon appends '.session' when absent, so every consumer - the privacy
    assertion, the git check, the lock file - has to agree on the real filename.
    Pure: creates nothing, so callers can check a path before it exists."""
    return p if p.name.endswith(SUFFIX) else p.with_name(p.name + SUFFIX)


def prepare_session_path(p: Path) -> Path:
    """Create the session file 0600 *before* Telethon can create it 0644.

    sqlite3.connect() on a fresh path yields 0644 under a default umask, and the
    auth key is written during login - so a post-hoc chmod is too late. cli.main
    sets umask(0o077) first; this is the belt to that braces.
    """
    p = with_session_suffix(p)
    p.parent.mkdir(parents=True, exist_ok=True)
    try:
        os.close(os.open(p, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600))
    except FileExistsError:
        os.chmod(p, 0o600)
    return p


def assert_session_private(p: Path) -> None:
    """Every sqlite sibling too: -journal, -wal and -shm hold the same secrets."""
    for f in sorted(p.parent.glob(p.name + "*")):
        mode = stat.S_IMODE(f.stat().st_mode)
        if mode & 0o077:
            raise SystemExit(f"insecure mode on {f}: {oct(mode)}")


def _nearest_existing_dir(p: Path) -> Path | None:
    """First existing ancestor directory, used as git's cwd.

    The target usually does not exist yet - an export root on a first run, or a
    session path under a directory we have not created. Running git from a
    nonexistent cwd raises FileNotFoundError before git can answer.
    """
    for candidate in [p, *p.parents]:
        if candidate.is_dir():
            return candidate
    return None


def _check_ignore(target: str | Path, cwd: Path) -> int:
    """git check-ignore rc: 0 = ignored, 1 = NOT ignored, 128 = not a work tree.
    Returns 0 when git is unavailable, so a missing git never blocks a run."""
    try:
        return subprocess.run(["git", "check-ignore", "-q", str(target)],
                              cwd=cwd, capture_output=True).returncode
    except (FileNotFoundError, NotADirectoryError):
        return 0    # no git installed, or the tree moved under us - not our problem


def assert_not_committable(p: Path, *, is_dir: bool = False) -> None:
    """Refuse a session file or export root that git would happily commit.

    Adding .gitignore patterns cannot help when --out and --session point
    anywhere, so this is checked at runtime against the actual path.

    is_dir must be True only for a path that will become a directory. The
    trailing-slash retry below is what makes a directory-only pattern match
    before the directory exists - and asking about a *file* that way would be
    unsound in the other direction: a `.gitignore` line of `creds.session/`
    would then excuse a file named `creds.session`, which git would commit.
    """
    target = p.absolute()
    cwd = _nearest_existing_dir(target)
    if cwd is None:
        return
    rc = _check_ignore(target, cwd)
    if rc == 1 and is_dir and not target.exists():
        # A directory-only pattern such as `exports/` does not match a path git
        # cannot see is a directory, so an export root would be refused on the
        # first run and accepted on every run after it. The trailing slash tells
        # git to read it as a directory - passed as a string, because Path drops
        # it. Verified empirically against git's exit codes.
        rc = _check_ignore(f"{target}{os.sep}", cwd)
    if rc == 1:
        raise SystemExit(
            f"refusing: {p} is inside a git repo and is not gitignored.\n"
            f"add it to .gitignore, or choose a path outside the repo.")


# --------------------------------------------------------------------------- #
# Flood-wait primitives - two, because one cannot cover both shapes
# --------------------------------------------------------------------------- #

async def _flood_sleep(e: FloodWaitError, max_wait_s: int | None,
                       where: str = "") -> None:
    """Sleep out one flood wait, or abort if it exceeds an explicit ceiling.

    max_wait_s is None by default: sleep however long Telegram demands. Always
    log the computed WAKE TIME, not just the duration - an unattended four-hour
    sleep has to read as a sleep rather than as a hang. That log line is the
    mitigation for the no-ceiling default.
    """
    if max_wait_s is not None and e.seconds > max_wait_s:
        raise Abort(f"flood wait {e.seconds}s exceeds --max-flood-wait {max_wait_s}s", 6)
    wake = datetime.now(UTC) + timedelta(seconds=e.seconds)
    log.warning("flood wait %ss%s - sleeping until %s",
                e.seconds, where, wake.isoformat(timespec="seconds"))
    await asyncio.sleep(e.seconds + random.uniform(1.0, 3.0))


async def with_flood_retry(make_coro: Callable[[], object], *,
                           max_wait_s: int | None = None):
    """Download path. Never tight-retry: a retry during an active wait is a
    fresh violation that escalates the next wait."""
    while True:
        try:
            return await make_coro()
        except FloodWaitError as e:
            await _flood_sleep(e, max_wait_s)


async def aiter_with_flood_retry(make_agen, *, start_id: int,
                                 max_wait_s: int | None = None):
    """Sweep path.

    iter_messages is consumed with `async for`, and Telethon raises
    FloodWaitError on the *next page fetch* - deep inside the loop, where a
    wrapper guarding only construction never sees it. Rebuilding the generator
    from the last id actually yielded is what keeps the sweep gap-free: with
    reverse=True, offset_id is an exclusive lower bound, so no message is
    skipped or replayed.
    """
    last = start_id
    while True:
        try:
            async for item in make_agen(last):
                last = item.id
                yield item
            return
        except FloodWaitError as e:
            await _flood_sleep(e, max_wait_s, where=f" mid-sweep after id {last}")


# --------------------------------------------------------------------------- #
# Credentials and client
# --------------------------------------------------------------------------- #

def load_api_credentials() -> tuple[int, str]:
    api_id, api_hash = os.environ.get("TG_API_ID"), os.environ.get("TG_API_HASH")
    if not api_id or not api_hash:
        raise Abort(
            "missing credentials: set TG_API_ID and TG_API_HASH.\n"
            "  create an app at https://my.telegram.org -> API development tools\n"
            "  then: export TG_API_ID=... TG_API_HASH=...", 1)
    try:
        return int(api_id), api_hash
    except ValueError:
        raise Abort(f"TG_API_ID must be an integer, got {api_id!r}", 1) from None


async def _login(client: TelegramClient) -> None:
    """Interactive first login. Later runs return immediately."""
    if await client.is_user_authorized():
        return
    phone = os.environ.get("TG_PHONE") or input("phone (+countrycode): ").strip()
    if not phone:
        raise Abort("no phone number given", 1)
    await client.send_code_request(phone)
    code = input("login code (sent via Telegram): ").strip()
    try:
        await client.sign_in(phone, code)
    except SessionPasswordNeededError:
        # 2FA password: getpass only. Never argv, never env, never a file.
        await client.sign_in(password=getpass("2FA password: "))


@asynccontextmanager
async def connected_client(session_path: Path, api_id: int,
                           api_hash: str) -> AsyncIterator[TelegramClient]:
    """Connected, authorized client that always disconnects.

    Telethon does not flush SQLite session state without disconnect(), so this
    is a context manager rather than a discipline.
    """
    # Check before creating: a refused path should not be left holding an empty
    # session file.
    session_path = with_session_suffix(session_path)
    assert_not_committable(session_path)
    session_path = prepare_session_path(session_path)
    client = TelegramClient(str(session_path), api_id, api_hash)
    client.flood_sleep_threshold = FLOOD_SLEEP_THRESHOLD
    await client.connect()
    try:
        await _login(client)
        assert_session_private(session_path)
        yield client
    finally:
        await client.disconnect()


async def resolve_entity(client: TelegramClient, spec: str):
    """Accept a numeric id, @username, or t.me link. Returns (entity, peer_id).

    peer_id is telethon.utils.get_peer_id(entity) - the canonical int that
    becomes the export directory name.
    """
    if "/+" in spec or "joinchat/" in spec:
        raise Abort(
            f"{spec} is a private invite link, which cannot be resolved without "
            f"joining.\njoin the group in a Telegram client first, then pass its "
            f"numeric id or @username.", 5)

    target = spec.strip()
    try:
        entity = await client.get_entity(int(target))
    except ValueError as e:
        # Not an int, or an int Telethon has never seen. Raw ids resolve only
        # from the session cache, so fall back to a dialog scan before giving up.
        try:
            entity = await client.get_entity(target)
        except ValueError:
            entity = await _find_in_dialogs(client, target)
            if entity is None:
                raise Abort(
                    f"cannot resolve group {spec!r}: {e}\n"
                    f"confirm this account is a member, and try the @username or "
                    f"the numeric id.", 5) from None
    return entity, utils.get_peer_id(entity)


async def _find_in_dialogs(client: TelegramClient, target: str):
    """Last resort for a numeric id absent from the session cache."""
    try:
        wanted = int(target)
    except ValueError:
        return None
    async for dialog in client.iter_dialogs():
        if utils.get_peer_id(dialog.entity) == wanted:
            return dialog.entity
    return None


def now_iso() -> str:
    return datetime.now(UTC).isoformat(timespec="seconds")
