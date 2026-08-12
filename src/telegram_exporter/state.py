"""The resume cursor, and nothing else.

state.py and sidecar.py are separate modules on purpose. The correctness-critical
part is the *ordering* between them - sidecar first, cursor second - and that
ordering belongs to the caller. Two objects in two modules makes the sequence
visible at one call site, and makes a "helpful" commit_and_append() impossible to
write without noticing that it destroys Invariant 2.
"""

from __future__ import annotations

import json
import logging
import os
from pathlib import Path

from .session import Abort, now_iso
from .traversal import Filters

log = logging.getLogger("telegram_exporter.state")

STATE_NAME = ".export-state.json"
VERSION = 1


def fsync_dir(path: Path) -> None:
    """A rename is not durable until its directory entry is.

    Lives here rather than in downloader.py because durability is what this
    module exists for; downloader.py imports it for the post-directory fsync that
    has to happen before the cursor claims a post is done.
    """
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


class State:
    """cursor_id means: every post with max_message_id <= cursor_id has been
    handled *under these filters*.

    One cursor field, not two. The sweep is a generator driven by the sink at
    concurrency 1, so it can never run ahead of the downloader - a second
    "swept_to" field would invent a divergence to manage.
    """

    def __init__(self, path: Path, data: dict) -> None:
        self.path = path
        self.data = data

    # ---- construction -------------------------------------------------- #

    @classmethod
    def path_for(cls, root: Path) -> Path:
        return root / STATE_NAME

    @classmethod
    def read(cls, root: Path) -> dict | None:
        """Read-only load for --dry-run. Never creates anything."""
        p = cls.path_for(root)
        if not p.exists():
            return None
        try:
            return json.loads(p.read_text())
        except (json.JSONDecodeError, OSError) as e:
            raise Abort(f"cannot read {p}: {e}\n"
                        f"inspect it, or use --reset-state to start over", 7) from None

    @classmethod
    def open(cls, root: Path, *, chat_id: int, chat_title: str,
             filters: Filters, reset: bool = False) -> State:
        """Load and validate, or create. Refuses a mismatch rather than guessing."""
        path = cls.path_for(root)
        existing = None if reset else cls.read(root)

        if existing is not None:
            _assert_compatible(existing, chat_id=chat_id, filters=filters)
            existing["chat_title"] = chat_title      # titles change; that is fine
            existing["completed_at"] = None          # this run has not finished yet
            existing["updated_at"] = now_iso()
            state = cls(path, existing)
        else:
            state = cls(path, {
                "version": VERSION,
                "chat_id": chat_id,
                "chat_title": chat_title,
                "filters": filters.to_state(),
                "cursor_id": 0,
                "completed_at": None,
                "started_at": now_iso(),
                "updated_at": now_iso(),
            })
        state.save()
        return state

    # ---- accessors ------------------------------------------------------ #

    @property
    def cursor_id(self) -> int:
        return int(self.data["cursor_id"])

    # ---- mutation ------------------------------------------------------- #

    def commit(self, cursor_id: int) -> None:
        """Invariant 2: only ever called after a COMPLETE post, and unreachable
        from every error exit, so the cursor can never point past in-flight work."""
        if cursor_id < self.cursor_id:
            log.debug("ignoring cursor regression %s -> %s", self.cursor_id, cursor_id)
            return
        self.data["cursor_id"] = int(cursor_id)
        self.save()

    def mark_completed(self) -> None:
        """Set only on clean exhaustion of the sweep. The only way to answer
        "did my 20-hour export finish?" without guessing."""
        self.data["completed_at"] = now_iso()
        self.save()

    def save(self) -> None:
        """Atomic: tmp -> fsync -> replace -> fsync(dir). A half-written state
        file would be indistinguishable from a corrupted cursor."""
        self.data["updated_at"] = now_iso()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        tmp = self.path.with_name(self.path.name + ".tmp")
        with open(tmp, "w") as f:
            json.dump(self.data, f, indent=2, sort_keys=True)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, self.path)
        fsync_dir(self.path.parent)


def _assert_compatible(existing: dict, *, chat_id: int, filters: Filters) -> None:
    """Invariant 3: filters are stored verbatim, and a mismatch names what changed.

    A sha256 fingerprint would be smaller to store and useless here: the whole
    point is to *print the diff*, and a digest is one-way.

    --limit is deliberately absent from the filter set. It changes when we stop,
    not what a post contains, so including it would let a `--limit 20` test run
    poison every later full run.
    """
    if int(existing.get("chat_id", 0)) != chat_id:
        raise Abort(
            f"chat mismatch: state file belongs to chat {existing.get('chat_id')}, "
            f"not {chat_id}\nexport each group to its own --out directory", 7)

    was, now = existing.get("filters") or {}, filters.to_state()
    changed = [k for k in sorted(set(was) | set(now)) if was.get(k) != now.get(k)]
    if changed:
        lines = "\n".join(f"  {k+':':12} {_show(was.get(k))} -> {_show(now.get(k))}"
                          for k in changed)
        raise Abort(
            f"filter mismatch:\n{lines}\n"
            f"the cursor means 'handled through here under these filters'.\n"
            f"re-run with the original filters, or --reset-state to start over", 7)


def _show(v: object) -> str:
    return "(none)" if v is None else repr(v)
