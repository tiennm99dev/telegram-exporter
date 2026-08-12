"""Command line entry point: process rails, argument parsing, dispatch.

The rails come first and in this order for reasons that are not stylistic:
umask before anything can create a file, logging policy before anything can log a
credential, the git-safety assertion before anything writes into a repo, and the
lock before the destructive .part sweep.
"""

from __future__ import annotations

import argparse
import asyncio
import fcntl
import logging
import os
import socket
import sys
from contextlib import contextmanager
from pathlib import Path

from . import paths
from .downloader import (
    FATAL_ERRNO,
    Config,
    Fetcher,
    run_download,
    sweep_part_files,
    write_title,
)
from .estimate import EstimateSink, resume_from, run_estimate
from .session import (
    Abort,
    assert_not_committable,
    connected_client,
    default_session_path,
    load_api_credentials,
    now_iso,
    resolve_entity,
    with_session_suffix,
)
from .sidecar import Sidecar
from .state import State
from .traversal import TraversalError, build_filters, iter_posts, parse_size

log = logging.getLogger("telegram_exporter.cli")

LOCK_NAME = ".export.lock"
DEFAULT_MIN_FREE = "2GiB"


def configure_logging(verbose: bool) -> None:
    """--verbose means *our* logger only.

    telethon's request logging carries api_id, phone_number and
    phone_code_hash, so its floor is unconditional and there is deliberately no
    --debug-telethon flag. The root level is never touched.
    """
    logging.basicConfig(level=logging.INFO,
                        format="%(asctime)s %(levelname)s %(message)s")
    logging.getLogger("telegram_exporter").setLevel(
        logging.DEBUG if verbose else logging.INFO)
    logging.getLogger("telethon").setLevel(logging.WARNING)
    logging.getLogger("asyncio").setLevel(logging.WARNING)


@contextmanager
def exclusive(path: Path):
    """Single-instance lock, per export root and per session.

    flock releases on process death, kill -9 included, so there is no stale-lock
    cleanup to get wrong. Contention fails fast with exit 3 - no wait, no
    --force. The message names pid, host and start time so that "did it hang?"
    gets an answer instead of a second process.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    f = open(path, "a+")
    try:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        f.seek(0)
        holder = f.read().strip() or "?"
        f.close()
        raise Abort(
            f"busy: another tg-export holds {path} [{holder}]\n"
            f"if you meant to export a second group at the same time, give it its "
            f"own --session too: one SQLite session cannot serve two clients.", 3
        ) from None
    f.seek(0)
    f.truncate()
    f.write(f"pid={os.getpid()} host={socket.gethostname()} started={now_iso()}\n")
    f.flush()
    try:
        yield
    finally:
        f.close()


def _non_negative(text: str) -> int:
    """A negative --limit is the one input where the estimator and the real run
    would disagree (one stops before counting, the other after one post), so it
    is rejected at the boundary rather than handled twice."""
    value = int(text)
    if value < 0:
        raise argparse.ArgumentTypeError(f"must be 0 or greater, got {value}")
    return value


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="tg-export",
        description="Export all media from a Telegram group, grouped by post.",
        epilog="credentials: export TG_API_ID and TG_API_HASH "
               "(create an app at https://my.telegram.org). "
               "The login code and any 2FA password are prompted for, never read "
               "from the environment.")
    p.add_argument("--group", required=True,
                   help="numeric id, @username, or t.me link")
    p.add_argument("--out", required=True, metavar="DIR",
                   help="parent directory; the export goes in <DIR>/g<chat_id>")
    p.add_argument("--dry-run", action="store_true",
                   help="report what a real run would fetch, write nothing")
    p.add_argument("--since", metavar="YYYY-MM-DD", help="messages on or after (UTC)")
    p.add_argument("--until", metavar="YYYY-MM-DD", help="messages on or before (UTC)")
    p.add_argument("--types", metavar="LIST",
                   help="comma-separated: photo,video,document,audio,voice")
    p.add_argument("--max-size", metavar="SIZE",
                   help="skip media larger than this, e.g. 100MB")
    p.add_argument("--include-text", action="store_true",
                   help="also record text-only messages in messages.jsonl with an "
                        "empty files[]; part of the stored filter set")
    p.add_argument("--limit", type=_non_negative, metavar="N",
                   help="stop after N posts that contain files (0 = connect only)")
    p.add_argument("--session", metavar="PATH",
                   help=f"session file (default: {default_session_path()})")
    p.add_argument("--reset-state", action="store_true",
                   help="discard the cursor and rotate messages.jsonl; keeps "
                        "downloaded files")
    p.add_argument("--min-free", default=DEFAULT_MIN_FREE, metavar="SIZE",
                   help=f"disk kept free (default {DEFAULT_MIN_FREE})")
    p.add_argument("--max-flood-wait", type=int, metavar="SECONDS",
                   help="exit resumably (code 6) instead of sleeping through a "
                        "flood wait longer than this; unset means sleep as long "
                        "as Telegram demands")
    p.add_argument("--verbose", action="store_true",
                   help="debug logging for this tool only, never for telethon")
    return p


async def _run(args) -> int:
    api_id, api_hash = load_api_credentials()
    filters = build_filters(types=args.types, since=args.since, until=args.until,
                            max_size=args.max_size, include_text=args.include_text)
    min_free = parse_size(args.min_free)
    session_path = with_session_suffix(
        Path(args.session).expanduser() if args.session else default_session_path())

    async with connected_client(session_path, api_id, api_hash) as client:
        entity, peer_id = await resolve_entity(client, args.group)
        title = (getattr(entity, "title", None)
                 or getattr(entity, "username", None) or str(peer_id))
        root = paths.export_root(Path(args.out).expanduser(), peer_id)
        assert_not_committable(root, is_dir=True)
        log.info("group %r (id %s) -> %s", title, peer_id, root)

        if args.dry_run:
            return await _dry_run(client, entity, root=root, title=title,
                                  peer_id=peer_id, filters=filters,
                                  min_free=min_free, args=args)
        return await _real_run(client, entity, root=root, title=title,
                               peer_id=peer_id, filters=filters,
                               min_free=min_free, session_path=session_path,
                               args=args)


async def _dry_run(client, entity, *, root: Path, title: str, peer_id: int,
                   filters, min_free: int, args) -> int:
    """Takes no lock, creates no directories, mutates no state - so it can run
    while a real export is in progress."""
    start = resume_from(root, filters)
    posts = iter_posts(client, entity, after_id=start, filters=filters,
                       max_flood_wait=args.max_flood_wait)
    return await run_estimate(posts, sink=EstimateSink(), root=root, title=title,
                              peer_id=peer_id, filters=filters, start_id=start,
                              min_free=min_free, limit=args.limit)


async def _real_run(client, entity, *, root: Path, title: str, peer_id: int,
                    filters, min_free: int, session_path: Path, args) -> int:
    root.mkdir(parents=True, exist_ok=True)
    session_lock = session_path.with_name(session_path.name + ".lock")

    # Both locks before the .part sweep: the sweep is the destructive operation,
    # so locking after it would lock after the damage. Per-root rather than
    # global, because exporting two different groups concurrently is legitimate.
    with exclusive(root / LOCK_NAME), exclusive(session_lock):
        sweep_part_files(root)
        write_title(root, title)

        # State first: it refuses a filter or chat mismatch (exit 7) before the
        # sidecar is touched.
        state = State.open(root, chat_id=peer_id, chat_title=title,
                           filters=filters, reset=args.reset_state)
        sidecar = Sidecar(root)
        if args.reset_state:
            sidecar.rotate()

        with sidecar:
            posts = iter_posts(client, entity, after_id=state.cursor_id,
                               filters=filters, max_flood_wait=args.max_flood_wait)
            totals = await run_download(
                posts,
                fetcher=Fetcher(client, entity, args.max_flood_wait),
                state=state, sidecar=sidecar, root=root,
                # The flood ceiling reaches the network through the two
                # primitives, not through Config: Fetcher above for the download
                # path, iter_posts for the sweep.
                cfg=Config(min_free=min_free, disk_path=root),
                limit=args.limit)
    log.info("done: %s", totals.summary())
    return 0


def main() -> int:
    os.umask(0o077)   # FIRST statement: covers the session file and every sqlite
                      # sibling, the export tree, messages.jsonl, .part files,
                      # the state file and the lock - one line, no per-file chmod.
    args = build_parser().parse_args()
    configure_logging(args.verbose)
    try:
        return asyncio.run(_run(args))
    except Abort as a:
        log.error("%s", a.reason)
        return a.code
    except TraversalError as e:
        # An album split or a non-ascending sweep. Loud on purpose: the export on
        # disk cannot be trusted, and the cursor is where the last good post was.
        log.error("%s", e)
        return 1
    except (ConnectionError, TimeoutError) as e:
        # Before the OSError clause: both subclass it, and the sweep path has no
        # retry of its own, so a mid-sweep reset arrives here. Calling that
        # "filesystem" would send the operator to the wrong place entirely.
        log.error("network: %s - the cursor is intact, re-run to resume", e)
        return 1
    except OSError as e:
        # The download path maps these itself; this catches the same failures
        # arriving from the sidecar or state writes, so disk exhaustion exits 3
        # there too instead of printing a traceback.
        code = 3 if e.errno in FATAL_ERRNO else 1
        log.error("filesystem: %s", e)
        return code
    except ValueError as e:                  # bad --types/--since/--max-size value
        log.error("%s", e)
        return 1
    except KeyboardInterrupt:
        log.info("interrupted - re-run the same command to resume")
        return 130
    except Exception:
        # Last resort, so no failure can exit outside the documented contract.
        # The traceback is still printed - CPython does not put locals in it, so
        # this cannot leak api_hash or a phone number - but the exit code stays
        # meaningful and the operator is told the cursor is safe.
        log.exception("unexpected error - the cursor is intact, re-run to resume")
        return 1


if __name__ == "__main__":
    sys.exit(main())
