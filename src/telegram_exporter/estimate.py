"""--dry-run: sweep, download nothing, report what a real run would fetch *from
where it would actually start*.

run_estimate drives the same iter_posts generator with the same Filters as
run_download. That shared generator is the anti-drift property: an estimator with
its own traversal would report numbers that do not match reality. There is no
Sink Protocol - two implementations with one call site and no shared behavior is
indirection describing a for loop - so both run modes own a plain `async for`,
and the duplication is two lines.
"""

from __future__ import annotations

import shutil
from dataclasses import dataclass, field
from pathlib import Path

from .downloader import human_bytes
from .media import has_media, media_kind, media_size
from .state import State
from .traversal import Filters

# Photo .size describes the size *variant* Telethon selected, so photo totals are
# approximate. Documents, video and audio carry an exact document.size.
_LABELS = {"photo": "photos", "video": "videos", "document": "documents",
           "audio": "audio files", "voice": "voice notes"}


@dataclass
class EstimateSink:
    """Counts posts *with files*, files, and bytes by kind.

    Empty posts (a fully filtered group, which iter_posts yields to keep the
    cursor advancing) and text-only posts under --include-text are not counted,
    on both sides of any comparison - so the dry-run and real-run reconciliation
    in phase 7 compares like with like.
    """

    posts: int = 0
    files: int = 0
    by_kind: dict[str, list[int]] = field(default_factory=dict)   # kind -> [count, bytes]
    unknown_size_files: int = 0

    def add(self, post) -> None:
        media = [m for m in post.messages if has_media(m)]
        if not media:
            return
        self.posts += 1
        for msg in media:
            self.files += 1
            kind = media_kind(msg)            # has_media guarantees this is set
            row = self.by_kind.setdefault(kind, [0, 0])
            row[0] += 1
            size = media_size(msg)
            if size is None:
                # Never coerced to 0: an unbounded estimate error must be visible.
                self.unknown_size_files += 1
            else:
                row[1] += size

    @property
    def total_bytes(self) -> int:
        return sum(row[1] for row in self.by_kind.values())


def _measurable_dir(p: Path) -> Path:
    """Nearest existing ancestor - dry-run must not create the export root just
    to ask how much space is left."""
    for candidate in [p, *p.parents]:
        if candidate.is_dir():
            return candidate
    return Path.cwd()


def describe_filters(filters: Filters) -> str:
    # `is not None` throughout: --max-size 0 is an active filter that drops
    # everything, and printing it as "(none)" would misdescribe the run.
    parts = []
    if filters.types is not None:
        parts.append("types=" + ",".join(sorted(filters.types)))
    if filters.since is not None:
        parts.append(f"since={filters.since.date()}")
    if filters.until is not None:
        parts.append(f"until={filters.until.date()}")
    if filters.max_size is not None:
        parts.append(f"max-size={human_bytes(filters.max_size)}")
    if filters.include_text:
        parts.append("include-text")
    return "  ".join(parts) or "(none)"


def stored_filters_differ(root: Path, filters: Filters) -> bool:
    """Whether a real run would refuse (exit 7) rather than resume."""
    data = State.read(root)
    return bool(data) and data.get("filters") != filters.to_state()


def resume_from(root: Path, filters: Filters) -> int:
    """Start where the real run would.

    Without this, `tg-export --dry-run && tg-export` fails *closed* on every
    resumed run: a 38 GB group with 30 GB already downloaded and 11 GB free would
    re-measure the whole 38 GB, exit 2, and refuse to fetch the 8 GB that fits
    comfortably - leaving the user no option but to bypass the guard they were
    told to trust.

    Deliberately not scanning the export dir to subtract bytes already on disk:
    with a cursor-based sweep everything before the cursor is already excluded,
    and the residue is a handful of failed files and one partial post.
    """
    data = State.read(root)
    if not data:
        return 0
    if data.get("filters") != filters.to_state():
        return 0                      # different filters: the real run would refuse
    return int(data.get("cursor_id", 0))


def _mode_line(root: Path, filters: Filters, start_id: int) -> str:
    """Say which run this describes.

    When the stored filters differ, the numbers below are honest for a *fresh*
    export but misleading as "remaining work", because a real run would refuse
    with exit 7 until --reset-state. Saying so beats printing a total the next
    command cannot act on.
    """
    if start_id:
        return f"resume from message {start_id}"
    if stored_filters_differ(root, filters):
        return ("full history - stored filters differ, so a real run would refuse "
                "(exit 7) until --reset-state")
    return "full history (no prior state)"


async def run_estimate(posts, *, sink: EstimateSink, root: Path,
                       title: str, peer_id: int, filters: Filters,
                       start_id: int, min_free: int,
                       limit: int | None = None) -> int:
    """Consume the shared traversal, print the report, return the exit code.

    The verdict is advisory. Phase 5's per-file check_disk is the sole
    enforcement mechanism, so a SHORT verdict does not mean the run is unsafe -
    it means the run will stop partway with a clean, resumable cursor.
    """
    if limit != 0:
        async for post in posts:
            sink.add(post)
            # Same stop rule as run_download, so `--dry-run --limit N` and
            # `--limit N` from the same start describe the same work.
            if limit is not None and sink.posts >= limit:
                break

    free = shutil.disk_usage(_measurable_dir(root)).free
    total = sink.total_bytes
    need = total + min_free

    print(f"Group:      {title or '(untitled)'}  (id {peer_id})")
    print(f"Filters:    {describe_filters(filters)}")
    print(f"Mode:       {_mode_line(root, filters, start_id)}")
    print()
    print(f"Remaining:  {sink.posts:,} posts / {sink.files:,} files")
    for kind, (count, size) in sorted(sink.by_kind.items()):
        note = "   (approximate - size variant)" if kind == "photo" else ""
        print(f"  {_LABELS.get(kind, kind):<14}{count:>6,} files   "
              f"{human_bytes(size):>12}{note}")
    if sink.unknown_size_files:
        print(f"  {'unknown size':<14}{sink.unknown_size_files:>6,} files   "
              f"{'(not counted)':>12}")
    print()
    print(f"Total:      {human_bytes(total)}")
    print(f"Free:       {human_bytes(free)}   Reserve: {human_bytes(min_free)}")

    if free < need:
        print(f"Verdict:    SHORT by {human_bytes(need - free)}")
        print(f"            advisory - the run would stop partway with a "
              f"resumable cursor, not corrupt anything")
        return 2
    print(f"Verdict:    FITS ({human_bytes(free - need)} to spare)")
    return 0
