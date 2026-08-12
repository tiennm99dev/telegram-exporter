"""The download path: fetch sequentially, write durably, dedupe, record.

Governing principle: **the filesystem is the state; the state file is a hint; the
sidecar is the log.** Everything here follows from it.

Telethon has no byte-level resume - download_media writes from offset 0 every
call - so resumability lives at *message* granularity, and completeness is proven
by arrival at the target path:

    per file:  write -> flush -> fsync(fd) -> os.replace(tmp, target)
    per post:  fsync(post_dir) -> sidecar.append + fsync -> state.commit

With fsync before os.replace, a file at the target path can only have arrived
fully downloaded. That is what licenses existence-based dedupe below.
"""

from __future__ import annotations

import asyncio
import errno
import logging
import os
import shutil
import time
from dataclasses import dataclass
from pathlib import Path

from telethon.errors import (
    AuthKeyDuplicatedError,
    AuthKeyUnregisteredError,
    ChannelInvalidError,
    ChannelPrivateError,
    ChatForbiddenError,
    FileIdInvalidError,
    FileReferenceExpiredError,
    FilerefUpgradeNeededError,
    MediaEmptyError,
    ServerError,
    SessionRevokedError,
    UserDeactivatedBanError,
)
from telethon.errors import common as telethon_common

from . import paths
from .media import file_ext, has_media, media_kind, media_size
from .session import Abort, with_flood_retry
from .state import fsync_dir

log = logging.getLogger("telegram_exporter.downloader")

TRANSIENT_ERRNO = {errno.EAGAIN, errno.EINTR, errno.EIO, errno.ETIMEDOUT,
                   errno.ECONNRESET}
FATAL_ERRNO = {errno.ENOSPC, errno.EDQUOT, errno.EROFS, errno.EACCES}

MAX_ATTEMPTS = 3
PART_SUFFIX = ".part"
TITLE_NAME = "title.txt"

# Commit the cursor at least this often even across a long filtered region, so a
# 100k-message sweep does not become 100k state writes.
COMMIT_INTERVAL_S = 5.0

DOWNLOADED, SKIPPED, FAILED = "DOWNLOADED", "SKIPPED", "FAILED"

# Network trouble is retried; the unavailable-media errors are skipped; the
# access and auth errors end the run. FloodWaitError never appears here - it is
# owned by with_flood_retry, because a retry during an active wait is counted as
# a fresh violation and escalates the next wait.
#
# ServerError rather than only RpcCallFailError: the latter is one leaf of it, so
# a plain Telegram -500 would otherwise escape. The telethon.errors.common set
# derives from Exception and BufferError, not OSError - a corrupt packet or a
# checksum failure on a flaky link is transient, but nothing below OSError would
# have caught it, and an unhandled one ends a 20-hour unattended run with a
# traceback.
_NETWORK_ERRORS = (ConnectionError, TimeoutError, asyncio.TimeoutError,
                   asyncio.IncompleteReadError, ServerError,
                   telethon_common.InvalidBufferError,
                   telethon_common.InvalidChecksumError,
                   telethon_common.TypeNotFoundError,
                   telethon_common.SecurityError,
                   telethon_common.BadMessageError)
_AUTH_ERRORS = (AuthKeyUnregisteredError, AuthKeyDuplicatedError,
                SessionRevokedError, UserDeactivatedBanError)
_ACCESS_ERRORS = (ChannelPrivateError, ChatForbiddenError, ChannelInvalidError)
_UNAVAILABLE_ERRORS = (MediaEmptyError, FileIdInvalidError, FilerefUpgradeNeededError)


def backoff(attempt: int) -> float:
    return 5.0 * 2 ** (attempt - 1)          # 5, 10, 20 s


@dataclass(frozen=True)
class Config:
    """Everything download_one needs from the CLI, so it takes no globals."""

    min_free: int                            # bytes held in reserve, default 2 GiB
    disk_path: Path                          # filesystem to measure - the export root
    # No max_flood_wait here: the ceiling belongs to the two flood primitives,
    # which Fetcher owns. A copy on this object would read as though check_disk
    # and download_one honored it.


@dataclass
class Result:
    """What happened to one message's media, as the sidecar records it."""

    message_id: int
    status: str
    path: Path | None = None
    size: int | None = None
    declared_size: int | None = None
    name: str | None = None
    mime: str | None = None
    kind: str | None = None
    error: str | None = None

    def as_record(self, root: Path) -> dict:
        record = {
            "path": str(self.path.relative_to(root)),
            "size": self.size,
            "name": self.name,
            "mime": self.mime,
            "kind": self.kind,
        }
        # declared_size appears only when it differs, which incidentally
        # accumulates empirical data on photo-variant accuracy.
        if self.declared_size is not None and self.declared_size != self.size:
            record["declared_size"] = self.declared_size
        return record


@dataclass
class Totals:
    """Per-run, in-memory counters. Deliberately not persisted: counters in the
    state file double-count on every resume, because a re-processed album
    re-increments them."""

    posts: int = 0
    downloaded: int = 0
    skipped: int = 0
    failed: int = 0
    bytes_downloaded: int = 0

    def summary(self) -> str:
        line = (f"{self.posts} posts, {self.downloaded} downloaded "
                f"({human_bytes(self.bytes_downloaded)}), {self.skipped} already present")
        if self.failed:
            line += f", {self.failed} files failed (see messages.jsonl)"
        return line


def human_bytes(n: int) -> str:
    step = 1024.0
    value = float(n)
    for unit in ("B", "KiB", "MiB", "GiB", "TiB"):
        if value < step or unit == "TiB":
            return f"{value:,.1f} {unit}" if unit != "B" else f"{int(value):,} B"
        value /= step
    raise AssertionError("unreachable: the loop returns at TiB")


# --------------------------------------------------------------------------- #
# Fetcher - the only part that touches the network, so tests can replace it
# --------------------------------------------------------------------------- #

class Fetcher:
    """Wraps the two client calls download_one needs, each behind flood retry."""

    def __init__(self, client, entity, max_flood_wait: int | None = None) -> None:
        self.client = client
        self.entity = entity
        self.max_flood_wait = max_flood_wait

    async def fetch(self, msg, fh) -> None:
        await with_flood_retry(lambda: self.client.download_media(msg, file=fh),
                               max_wait_s=self.max_flood_wait)

    async def refresh(self, message_id: int):
        """Re-fetch a message to renew an expired file reference."""
        return await with_flood_retry(
            lambda: self.client.get_messages(self.entity, ids=message_id),
            max_wait_s=self.max_flood_wait)


# --------------------------------------------------------------------------- #
# Per-file decisions
# --------------------------------------------------------------------------- #

def check_disk(msg, cfg: Config) -> None:
    """Refuse before the first byte, comparing against *this file's* size.

    One statvfs is microseconds, unmeasurable against a multi-second download at
    concurrency 1. Polling every N files would optimize a cost that does not
    exist while opening a blind spot: a single Telegram file reaches 2-4 GB, so
    ~100 GB could be written between two polls. An unknown size counts as 0 and
    is absorbed by the reserve, which is why the reserve defaults to 2 GiB - one
    entire unknown-size file at the non-premium upload cap.
    """
    need = (media_size(msg) or 0) + cfg.min_free
    free = shutil.disk_usage(cfg.disk_path).free
    if free < need:
        raise Abort(f"insufficient disk on {cfg.disk_path}: need "
                    f"{human_bytes(need)} (file + {human_bytes(cfg.min_free)} "
                    f"reserve), have {human_bytes(free)}", 3)


def validate(tmp: Path, msg) -> bool:
    """Advisory only - never raises. True means accept.

    Documents, video and audio carry an exact document.size, so a mismatch there
    is real truncation. Photos are the only approximate case: .size describes the
    size *variant* Telethon selected. An exact-equality gate would delete every
    completed photo on resume and then raise, which is why this is advisory and
    why dedupe below tests existence rather than size.
    """
    actual, declared = tmp.stat().st_size, media_size(msg)
    if actual == 0:
        # Nothing arrived. download_media returns without writing when the media
        # resolves to an empty variant, and the photo excuse below would
        # otherwise rename a 0-byte file to the target - where the dedupe rule
        # ("zero bytes is never a complete download") could never see it again,
        # because a completed export does not revisit existing targets.
        log.warning("message %s produced 0 bytes - retrying", msg.id)
        return False
    if declared is None or actual == declared:
        return True
    if media_kind(msg) == "photo":
        log.debug("photo %s declared %s, got %s - variant size, accepted",
                  msg.id, declared, actual)
        return True
    log.warning("size mismatch on message %s: declared %s, got %s - retrying",
                msg.id, declared, actual)
    return False


def sweep_part_files(root: Path) -> int:
    """Remove stray .part files left by a killed run.

    Destructive, so the caller must already hold the export lock: sweeping first
    and locking second would lock after the damage.
    """
    removed = 0
    for stray in root.rglob(f"*{PART_SUFFIX}"):
        stray.unlink(missing_ok=True)
        removed += 1
    if removed:
        log.info("swept %d stray %s file(s)", removed, PART_SUFFIX)
    return removed


async def download_one(fetcher, post_dir_path: Path, msg, *, cfg: Config) -> Result:
    """Download one message's media into post_dir_path.

    Every exit is one of: DOWNLOADED, SKIPPED, FAILED (recorded, run continues),
    or Abort (run ends with a correct cursor). There is no path that lets an
    unhandled exception kill an unattended 20-hour export.
    """
    try:
        target = paths.safe_join(post_dir_path, paths.derive_filename(msg))
    except (ValueError, TypeError) as e:
        # sanitize should make this unreachable, so reaching it means sanitize has
        # a hole worth finding - hence ERROR, not a silent repair. But one
        # hostile filename must not be able to end the run: without the cursor
        # advancing past this post, every later run would die on the same
        # message. Recorded in errors[] and skipped, like unavailable media.
        log.error("cannot build a safe path for message %s: %s - skipping", msg.id, e)
        return _result(msg, FAILED, error=f"unsafe filename: {e}")

    if target.exists():
        if target.stat().st_size > 0:
            return _result(msg, SKIPPED, target)
        # Zero bytes means ENOSPC junk, never a complete download - re-fetch.
        target.unlink()

    check_disk(msg, cfg)
    post_dir_path.mkdir(parents=True, exist_ok=True)
    tmp = target.with_name(target.name + PART_SUFFIX)
    refreshed = False

    for attempt in range(1, MAX_ATTEMPTS + 1):
        try:
            with open(tmp, "wb") as fh:
                await fetcher.fetch(msg, fh)
                fh.flush()
                os.fsync(fh.fileno())
            if not validate(tmp, msg):
                tmp.unlink(missing_ok=True)
                await asyncio.sleep(backoff(attempt))
                continue
            os.replace(tmp, target)
            return _result(msg, DOWNLOADED, target)

        # ---- SKIP: this message is unavailable; the run continues ---------- #
        except FileReferenceExpiredError:
            # References expire in hours, so a 20-hour run *will* hit this. One
            # refresh is the highest-value entry in this table.
            tmp.unlink(missing_ok=True)
            if refreshed:
                return _result(msg, FAILED, error="file reference expired twice")
            fresh = await fetcher.refresh(msg.id)
            refreshed = True
            if fresh is None or not has_media(fresh):
                return _result(msg, FAILED, error="message deleted")
            msg = fresh
            continue
        except _UNAVAILABLE_ERRORS as e:
            tmp.unlink(missing_ok=True)
            return _result(msg, FAILED,
                           error=f"media unavailable or expired: {type(e).__name__}")

        # ---- ABORT: the run cannot continue ------------------------------- #
        except _AUTH_ERRORS:
            tmp.unlink(missing_ok=True)
            raise Abort("session invalidated - delete the session file and re-login", 4)
        except _ACCESS_ERRORS:
            tmp.unlink(missing_ok=True)
            raise Abort("lost access to the group", 5)

        # ---- RETRY: network trouble --------------------------------------- #
        # Before the OSError clause on purpose: ConnectionError *is* an OSError,
        # and a ConnectionResetError raised by asyncio can carry errno=None,
        # which the fatal/transient errno test below would fall through to
        # "unexpected OS error" and abort a healthy run.
        except _NETWORK_ERRORS as e:
            tmp.unlink(missing_ok=True)
            log.warning("network error on message %s (attempt %d/%d): %s",
                        msg.id, attempt, MAX_ATTEMPTS, e)
            await asyncio.sleep(backoff(attempt))
            continue

        # ---- filesystem: fatal checked before transient ------------------- #
        except OSError as e:
            tmp.unlink(missing_ok=True)
            if e.errno in FATAL_ERRNO:
                raise Abort(f"filesystem: {e}", 3) from None
            if e.errno in TRANSIENT_ERRNO:
                await asyncio.sleep(backoff(attempt))
                continue
            raise Abort(f"unexpected OS error: {e}", 1) from None

    return _result(msg, FAILED, error=f"exhausted {MAX_ATTEMPTS} attempts")


def _result(msg, status: str, target: Path | None = None,
            error: str | None = None) -> Result:
    f = getattr(msg, "file", None)
    return Result(
        message_id=msg.id,
        status=status,
        path=target,
        size=target.stat().st_size if target is not None else None,
        declared_size=media_size(msg),
        name=getattr(f, "name", None) or f"{media_kind(msg)}{file_ext(msg)}",
        mime=getattr(f, "mime_type", None),
        kind=media_kind(msg),
        error=error,
    )


# --------------------------------------------------------------------------- #
# The sink loop - Invariant 2 by construction
# --------------------------------------------------------------------------- #

async def run_download(posts, *, fetcher, state, sidecar, root: Path,
                       cfg: Config, limit: int | None = None) -> Totals:
    """Drive the shared traversal, downloading as it goes.

    state.commit is reached only after a post is fully handled, and there is no
    commit on any error path - so the cursor can never point past in-flight work.
    "Commit the cursor and exit" is deliberately not a concept here; it was the
    mechanism by which the disk guard could have violated Invariant 2.

    Abort and KeyboardInterrupt propagate to cli.main, which owns exit codes.
    The cursor is already correct when they do.
    """
    totals = Totals()
    last_commit = time.monotonic()
    last_complete = state.cursor_id
    if limit == 0:
        return totals                        # connectivity check, no work requested

    async for post in posts:
        media = [m for m in post.messages if has_media(m)]
        results: list[Result] = []

        if media:
            target_dir = paths.post_dir(root, post.post_id)
            for msg in media:
                result = await download_one(fetcher, target_dir, msg, cfg=cfg)
                results.append(result)
                _tally(totals, result, post.post_id)
            if target_dir.exists():
                fsync_dir(target_dir)

        # A post may be empty (everything filtered out) or text-only under
        # --include-text; both still advance the cursor, and neither is counted.
        if post.messages:
            sidecar.append(post, results)
            sidecar.fsync()
        if media:
            totals.posts += 1

        # The post is now fully handled: files on disk, sidecar durable.
        last_complete = post.max_message_id
        wrote = any(r.status in (DOWNLOADED, FAILED) for r in results)
        if wrote or (time.monotonic() - last_commit) > COMMIT_INTERVAL_S:
            state.commit(last_complete)
            last_commit = time.monotonic()

        if limit is not None and totals.posts >= limit:
            log.info("--limit %d reached", limit)
            state.commit(last_complete)
            return totals

    # Flush the throttled tail before claiming the export finished. Without this,
    # a run ending on a stretch of filtered or text-only posts shorter than the
    # commit interval would record completed_at against a cursor still pointing
    # at the last post that happened to contain a file - and the next run would
    # re-sweep that whole tail to download nothing.
    state.commit(last_complete)
    state.mark_completed()
    return totals


def _tally(totals: Totals, result: Result, post_id: int) -> None:
    if result.status == DOWNLOADED:
        totals.downloaded += 1
        totals.bytes_downloaded += result.size or 0
        log.info("%s/%s  %s", post_id, result.path.name, human_bytes(result.size or 0))
    elif result.status == SKIPPED:
        totals.skipped += 1
        log.debug("%s/%s  already present", post_id, result.path.name)
    else:
        totals.failed += 1
        log.warning("%s message %s: %s", post_id, result.message_id, result.error)


def write_title(root: Path, title: str) -> None:
    """The human-readable group name lives here, as data - never as a path
    component. Written once; a later rename updates it without moving anything."""
    root.mkdir(parents=True, exist_ok=True)
    (root / TITLE_NAME).write_text((title or "") + "\n", encoding="utf-8")
