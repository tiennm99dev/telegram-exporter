"""The single traversal generator both run modes consume.

Chronological sweep, album buffering, filter application. Yields one Post per
logical post a human would recognize.

`run_download()` and `run_estimate()` drive *this* generator with the same
Filters. That shared generator - not any class hierarchy - is what guarantees the
dry-run estimate describes what a real run would download. A forked traversal
would drift and lie, so never fork it.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any

from .media import KINDS, media_kind, media_size
from .session import aiter_with_flood_retry

log = logging.getLogger("telegram_exporter.traversal")

# An album is capped at 10 members by Telegram; 20 is slack for a protocol
# change, and exceeding it means the grouping logic is wrong, not that Telegram
# got generous.
MAX_ALBUM_BUFFER = 20

_SIZE_UNITS = {"": 1, "B": 1, "K": 10**3, "KB": 10**3, "KIB": 2**10,
               "M": 10**6, "MB": 10**6, "MIB": 2**20,
               "G": 10**9, "GB": 10**9, "GIB": 2**30,
               "T": 10**12, "TB": 10**12, "TIB": 2**40}


class TraversalError(Exception):
    """The sweep is not the shape the whole design assumes. Never repaired: a
    wrong assumption here produces silent corruption that looks like success."""


class AlbumSplitError(TraversalError):
    """A grouped_id reopened after its run closed - the album was split across a
    non-adjacent boundary, so folder identity is no longer trustworthy."""


class SweepOrderError(TraversalError):
    """The sweep is not strictly ascending, or an album exceeded its bound.

    A real exception rather than a bare assert: `python -O` strips assertions,
    and these two tripwires guard the same class of silent corruption as
    AlbumSplitError sitting beside them.
    """


@dataclass(frozen=True)
class Post:
    """One logical post: a standalone message, or every member of one album.

    grouped_id is deliberately absent. The sidecar reads it off the message and
    folder identity uses post_id, so a field here would be a third copy of a
    fact with two owners already.
    """

    post_id: int              # lowest message id in the FULL group (pre-filter)
    messages: list[Any]       # filter-surviving members, ascending by id
    max_message_id: int       # highest id in the FULL group - the cursor value


@dataclass(frozen=True)
class Filters:
    """What a post *contains*. Stored verbatim in the state file, because the
    cursor means "handled through here under these filters"."""

    types: frozenset[str] | None = None
    since: datetime | None = None
    until: datetime | None = None
    max_size: int | None = None
    include_text: bool = False

    # Set once, so the unknown-size warning does not repeat per file.
    _warned: set[str] = field(default_factory=set, compare=False, repr=False)

    def to_state(self) -> dict:
        """The stored form. Verbatim and comparable - no digest, because two
        call sites have to *name* what changed, and a hash cannot.

        `is not None`, not truthiness: an empty set is a filter that drops
        everything, and storing it as "no filter" would let a later unfiltered
        run inherit an end-of-history cursor and report success having downloaded
        nothing. build_filters refuses to construct one, so this is the second
        line of defense on Invariant 3.
        """
        return {
            "types": sorted(self.types) if self.types is not None else None,
            "since": self.since.isoformat() if self.since else None,
            "until": self.until.isoformat() if self.until else None,
            "max_size": self.max_size,
            "include_text": self.include_text,
        }


def parse_size(text: str) -> int:
    """`100MB`, `2GiB`, `1500000`. Decimal units are powers of 10, `*iB` are
    powers of 2 - the same convention `ls -h` and `df -h` use."""
    raw = str(text).strip().replace(" ", "")
    i = 0
    while i < len(raw) and (raw[i].isdigit() or raw[i] == "."):
        i += 1
    number, unit = raw[:i], raw[i:].upper()
    if not number or unit not in _SIZE_UNITS:
        raise ValueError(f"cannot parse size {text!r}; try 100MB, 2GiB or a byte count")
    return int(float(number) * _SIZE_UNITS[unit])


def parse_date(text: str) -> datetime:
    """`--since`/`--until` as UTC-aware, because msg.date is UTC-aware and a
    naive comparison raises TypeError mid-sweep."""
    value = datetime.fromisoformat(str(text).strip())
    return value.replace(tzinfo=UTC) if value.tzinfo is None else value.astimezone(UTC)


def build_filters(*, types: str | None = None, since: str | None = None,
                  until: str | None = None, max_size: str | None = None,
                  include_text: bool = False) -> Filters:
    kinds = None
    if types:
        kinds = frozenset(t.strip().lower() for t in types.split(",") if t.strip())
        unknown = kinds - set(KINDS)
        if unknown:
            raise ValueError(f"unknown media types {sorted(unknown)}; "
                             f"choose from {', '.join(KINDS)}")
        if not kinds:
            # `--types " "` or `--types ,` - easy to produce from an unset shell
            # variable. Left alone it is a filter that drops every media kind,
            # which would sweep the whole history, download nothing, and commit
            # an end-of-history cursor.
            raise ValueError(f"--types {types!r} names no media kind; "
                             f"choose from {', '.join(KINDS)}, or omit the flag")
    return Filters(
        types=kinds,
        since=parse_date(since) if since else None,
        until=parse_date(until) if until else None,
        max_size=parse_size(max_size) if max_size else None,
        include_text=include_text,
    )


def keep(msg, filters: Filters) -> bool:
    """Whether this message survives the filters.

    Text-only messages are dropped unless --include-text, in which case they
    reach the sidecar with an empty files[] so captions and replies keep their
    surrounding conversation. They never create a post folder and never reach
    download_one.
    """
    kind = media_kind(msg)
    if kind is None:
        return filters.include_text
    if filters.types is not None and kind not in filters.types:
        return False
    date = getattr(msg, "date", None)
    if filters.since is not None and (date is None or date < filters.since):
        return False
    if filters.until is not None and (date is None or date > filters.until):
        return False
    if filters.max_size is not None:
        size = media_size(msg)
        if size is None:
            # Include and warn once. A silent drop violates the no-silent-gaps
            # ethos, and unknown-size media is pathological and near-always small.
            if "max_size_none" not in filters._warned:
                filters._warned.add("max_size_none")
                log.warning("some media reports no size; --max-size cannot apply "
                            "to it and it is being included")
        elif size > filters.max_size:
            return False
    return True


async def iter_posts(client, entity, *, after_id: int = 0,
                     filters: Filters, max_flood_wait: int | None = None):
    """Sweep oldest -> newest, yielding one Post per logical post.

    after_id is an exclusive lower bound: with reverse=True Telethon bumps
    offset_id by one before the first request and again per page (verified
    against telethon 1.44.0's _MessagesIter), so passing the last handled id
    resumes without replaying it. The explicit `msg.id <= after_id` guard makes
    the sweep correct even if that internal detail ever changes.
    """
    buf: list[Any] = []
    closed: set[int] = set()
    prev = 0

    def agen(since: int):
        return client.iter_messages(entity, reverse=True, offset_id=since)

    async for msg in aiter_with_flood_retry(agen, start_id=after_id,
                                            max_wait_s=max_flood_wait):
        if msg.id <= after_id:
            continue
        if msg.id <= prev:
            raise SweepOrderError(
                f"sweep not strictly ascending: {msg.id} after {prev}. "
                f"Album grouping and the cursor both assume ascending order.")
        prev = msg.id

        gid = getattr(msg, "grouped_id", None)
        if buf and (gid is None or gid != buf[0].grouped_id):
            yield _close(buf, closed, filters)
            buf = []
        if gid is None:
            yield _close([msg], closed, filters)
        else:
            buf.append(msg)
            if len(buf) > MAX_ALBUM_BUFFER:
                raise SweepOrderError(
                    f"album buffer exceeded {MAX_ALBUM_BUFFER} for grouped_id "
                    f"{gid} - grouping logic is wrong, refusing to keep buffering")
    if buf:
        yield _close(buf, closed, filters)


def _close(buf: list[Any], closed: set[int], filters: Filters) -> Post:
    """Turn a finished run of messages into a Post.

    Invariant 1 - identity is filter-invariant. post_id and max_message_id come
    from the complete grouped_id group, before filters drop any member, so
    `--types photo` and `--types video` address the same folder. Phase 4's
    message-id filenames close the same loop for files.
    """
    gid = getattr(buf[0], "grouped_id", None)
    if gid is not None:
        if gid in closed:
            raise AlbumSplitError(
                f"grouped_id {gid} reopened at msg {buf[0].id} after being closed - "
                f"album split across a non-adjacent boundary. Do not trust this export.")
        closed.add(gid)
    return Post(post_id=buf[0].id,
                messages=[m for m in buf if keep(m, filters)],
                max_message_id=buf[-1].id)
