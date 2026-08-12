"""messages.jsonl - the append-only log linking every file on disk to its message.

A record is an event: "the files written for this message in this run". The
correct consumer rule is therefore the UNION of all records for a message_id,
not last-write-wins. Last-write-wins is actively wrong: after a filter change it
would report a narrower file list than what is actually on disk.
"""

from __future__ import annotations

import json
import logging
import os
from datetime import UTC, datetime
from pathlib import Path

log = logging.getLogger("telegram_exporter.sidecar")

SIDECAR_NAME = "messages.jsonl"


class Sidecar:
    """Append-only JSONL writer. Opened for the life of the run."""

    def __init__(self, root: Path) -> None:
        self.root = root
        self.path = root / SIDECAR_NAME
        self._fh = None

    # ---- lifecycle ------------------------------------------------------ #

    def open(self) -> Sidecar:
        self.root.mkdir(parents=True, exist_ok=True)
        self.repair()
        self._fh = open(self.path, "a", encoding="utf-8")
        return self

    def close(self) -> None:
        if self._fh is not None:
            self._fh.close()
            self._fh = None

    def __enter__(self) -> Sidecar:
        return self.open()

    def __exit__(self, *exc) -> None:
        self.close()

    def repair(self) -> None:
        """Truncate a partial trailing line written when the host died mid-write.

        One unterminated line breaks every JSONL consumer, and the damage is
        always confined to the tail, so scanning back from EOF for the last
        newline is both sufficient and cheap on a 100k-line file.
        """
        if not self.path.exists() or self.path.stat().st_size == 0:
            return
        with open(self.path, "rb+") as f:
            f.seek(0, os.SEEK_END)
            end = f.tell()
            window = min(end, 1 << 20)
            f.seek(end - window)
            tail = f.read(window)
            cut = tail.rfind(b"\n")
            trailing = tail[cut + 1:]
            if not trailing:
                return                      # file ends on a newline: intact
            try:
                json.loads(trailing)
            except (json.JSONDecodeError, UnicodeDecodeError):
                offset = end - len(trailing)
                f.truncate(offset)
                log.warning("repaired %s: truncated %d trailing bytes of a partial "
                            "record at offset %d", self.path, len(trailing), offset)
                return
            # Valid JSON but no terminating newline - complete record, just add one.
            f.write(b"\n")

    def rotate(self) -> Path | None:
        """--reset-state rotates rather than deletes, so the current sidecar always
        describes exactly one filter regime while the old one stays inspectable."""
        if not self.path.exists():
            return None
        stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
        target = self.root / f"messages-{stamp}.jsonl"
        os.replace(self.path, target)
        log.info("rotated %s -> %s", self.path.name, target.name)
        return target

    # ---- writing -------------------------------------------------------- #

    def append(self, post, results) -> None:
        """One record per surviving message in the post.

        Text-only messages under --include-text produce the same shape with
        "files": [] - consumers distinguish media from context by files being
        empty, not by a separate record type.
        """
        by_message = {}
        for r in results:
            by_message.setdefault(r.message_id, []).append(r)

        for msg in post.messages:
            rs = by_message.get(msg.id, [])
            record = {
                "message_id": msg.id,
                "post_id": post.post_id,
                "grouped_id": getattr(msg, "grouped_id", None),
                "date": _iso(getattr(msg, "date", None)),
                "sender_id": getattr(msg, "sender_id", None),
                "sender_name": _sender_name(msg),
                "caption": (getattr(msg, "message", None) or None),
                "reply_to": _reply_to(msg),
                "files": [r.as_record(self.root) for r in rs if r.path is not None],
                "errors": [r.error for r in rs if r.error is not None],
            }
            self._fh.write(json.dumps(record, ensure_ascii=False) + "\n")

    def fsync(self) -> None:
        """Called before the cursor commits: the sidecar must be durable before
        the state file claims the post is done."""
        self._fh.flush()
        os.fsync(self._fh.fileno())


def _iso(value) -> str | None:
    return value.isoformat() if isinstance(value, datetime) else None


def _reply_to(msg) -> int | None:
    header = getattr(msg, "reply_to", None)
    return getattr(header, "reply_to_msg_id", None) if header is not None else None


def _sender_name(msg) -> str | None:
    """Only from an already-cached sender.

    Never an extra get_entity call: on a 100k-message sweep that is 100k extra
    RPCs and a flood ban, in exchange for a display string.
    """
    sender = getattr(msg, "sender", None)
    if sender is None:
        return None
    title = getattr(sender, "title", None)
    if title:
        return title
    parts = [getattr(sender, "first_name", None), getattr(sender, "last_name", None)]
    name = " ".join(p for p in parts if p)
    return name or getattr(sender, "username", None)
