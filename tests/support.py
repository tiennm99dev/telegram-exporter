"""Synthetic message stubs and async helpers, so every test runs offline.

media.py is duck-typed precisely so these stubs can exist: a test message needs
only the attributes its case is about.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from datetime import UTC, datetime

_DEFAULT_EXT = {"photo": ".jpg", "video": ".mp4", "audio": ".mp3",
                "voice": ".ogg", "document": ".bin"}
_MIME = {"photo": "image/jpeg", "video": "video/mp4", "audio": "audio/mpeg",
         "voice": "audio/ogg", "document": "application/octet-stream"}


@dataclass
class FakeFile:
    name: str | None = None
    ext: str | None = None
    size: int | None = None
    mime_type: str | None = None


class FakeMsg:
    """A Telethon Message as far as this codebase is concerned."""

    def __init__(self, id: int, *, kind: str | None = None,
                 grouped_id: int | None = None, date: datetime | None = None,
                 name: str | None = None, ext: str | None = None,
                 size: int | None = 1000, text: str | None = None,
                 web_preview: object | None = None, sender_id: int | None = None,
                 sender: object | None = None) -> None:
        self.id = id
        self.grouped_id = grouped_id
        self.date = date or datetime(2026, 1, 1, tzinfo=UTC)
        self.message = text
        self.sender_id = sender_id
        self.sender = sender
        self.reply_to = None
        self.web_preview = web_preview
        # Every media property media_kind() consults, default absent.
        self.photo = self.video = self.document = None
        self.audio = self.voice = self.gif = self.video_note = None
        self.file = None
        if kind is not None:
            setattr(self, kind, object())          # the property media_kind reads
            self.file = FakeFile(name=name,
                                 ext=ext or _DEFAULT_EXT.get(kind, ".bin"),
                                 size=size,
                                 mime_type=_MIME.get(kind))


class FakeClient:
    """Serves a fixed ascending message list, honoring offset_id exclusively -
    the semantics verified against telethon 1.44.0's _MessagesIter."""

    def __init__(self, messages: list[FakeMsg]) -> None:
        self.messages = messages
        self.calls: list[int] = []

    def iter_messages(self, entity, *, reverse=False, offset_id=0, **kwargs):
        assert reverse is True, "the sweep must always be chronological"
        self.calls.append(offset_id)

        async def gen():
            for m in self.messages:
                if m.id > offset_id:
                    yield m

        return gen()


def collect(agen) -> list:
    """Drain an async generator from a synchronous test."""

    async def drain():
        return [item async for item in agen]

    return asyncio.run(drain())


def run(coro):
    return asyncio.run(coro)


async def as_agen(items):
    for item in items:
        yield item
