"""Media classification and size, in one place because three consumers need it.

`--types` filtering, filename derivation, the download validator, the disk guard
and the estimator all have to agree on "what kind of media is this" and "how big
does the server say it is". Two copies of that answer would drift, and a drift
between the estimator and the downloader is exactly the lie the shared-traversal
design exists to prevent.

Duck-typed on purpose: every accessor uses getattr with a default, so unit tests
can build a stub message with just the attributes a case needs, offline.
"""

from __future__ import annotations

from pathlib import PurePosixPath

# The kinds `--types` accepts. "video" absorbs video notes and gifs; stickers
# fall through to "document" unless they carry a video attribute.
KINDS = ("photo", "video", "document", "audio", "voice")


def media_kind(msg) -> str | None:
    """The media kind, or None when the message carries no downloadable file.

    A link preview is deliberately not media. Telethon's `Message.photo` and
    `Message.document` also return the *web page preview's* photo or document,
    so classifying with those properties alone would make every message
    containing a URL look like an attachment - inflating counts and downloading
    thumbnails nobody asked for. Checking web_preview first excludes them.
    """
    if getattr(msg, "web_preview", None) is not None:
        return None
    if getattr(msg, "photo", None) is not None:
        return "photo"
    if getattr(msg, "voice", None) is not None:
        return "voice"                      # a voice note is an audio document
    if (getattr(msg, "video", None) is not None
            or getattr(msg, "video_note", None) is not None
            or getattr(msg, "gif", None) is not None):
        return "video"
    if getattr(msg, "audio", None) is not None:
        return "audio"
    if getattr(msg, "document", None) is not None:
        return "document"
    return None                             # text, poll, geo, contact, dice...


def has_media(msg) -> bool:
    return media_kind(msg) is not None


def media_size(msg) -> int | None:
    """Server-declared size in bytes, or None when the server did not say.

    Single source of truth. None is not zero, and each consumer has its own
    stated rule for it:

        --max-size filter   include, warn once (unknown-size media is
                            pathological and near-always small; a silent drop
                            would violate the no-silent-gaps ethos)
        download validator  accept - there is nothing to check against
        estimator           counted on a separate "unknown size" line, never
                            coerced to 0, because an unbounded estimate error
                            has to be visible
        disk guard          treated as 0, absorbed by the --min-free reserve

    Comparing `None <= limit` unguarded raises TypeError inside the sweep and
    kills a 20-hour export. That is the failure this helper exists to prevent.
    """
    f = getattr(msg, "file", None)
    if f is None:
        return None
    size = getattr(f, "size", None)
    return size if isinstance(size, int) else None


def file_stem(msg) -> str:
    """Stem for the filename, before sanitization. Untrusted: the name comes
    from DocumentAttributeFilename, which any group member controls."""
    name = getattr(getattr(msg, "file", None), "name", None)
    if name:
        stem = PurePosixPath(str(name)).stem
        if stem:
            return stem
    return media_kind(msg) or "media"       # photos and voice notes carry no name


def file_ext(msg) -> str:
    """Extension including the dot, falling back to .bin.

    Prefers the extension of the declared filename, then Telethon's mime-derived
    File.ext. Never empty, so a name is always recognizable on disk.
    """
    f = getattr(msg, "file", None)
    name = getattr(f, "name", None)
    if name:
        suffix = PurePosixPath(str(name)).suffix
        # Byte-counted, like the stem truncation in paths.sanitize: 16 multibyte
        # characters would be 48+ bytes, and "{id}_" + 200 stem bytes + that
        # would overflow ext4's 255-byte component limit.
        if suffix and len(suffix.encode()) <= 16:
            return suffix
    ext = getattr(f, "ext", None)
    if ext:
        return ext if str(ext).startswith(".") else f".{ext}"
    return ".bin"
