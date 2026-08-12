"""All path construction, from the export root down to the filename leaf.

Pure functions, no network, no I/O - which is why this is its own module rather
than a helper inside downloader.py, and why every rule below is table-tested
offline.

    The governing invariant: exactly one untrusted string ever becomes a path
    component - the filename leaf.

Every other component is an int, enforced by a type check, which makes
safe_join's "post_dir is trusted" precondition true by type rather than by
assumption. Two untrusted strings were in play before this module existed:

1. DocumentAttributeFilename, supplied by arbitrary group members.
   `../../../.ssh/authorized_keys` is a cheap, real attack.
2. The group title - server-supplied and editable by any admin. safe_join could
   never have caught that one, because the escape happens in a parent component
   before the join. It is fixed by never putting the title in a path at all.
"""

from __future__ import annotations

import os
import re
import unicodedata
from pathlib import Path

from .media import file_ext, file_stem, media_kind

# Characters removed outright rather than replaced: a NUL truncates the filename
# at the syscall boundary, so leaving a placeholder would only hide what
# happened.
#
#   Cc  C0 and C1 controls, including NUL and the ESC that starts an ANSI escape
#   Cf  format characters - U+202E RIGHT-TO-LEFT OVERRIDE is the one that
#       matters: `a<RLO>gpj.exe` renders as `a.exe.jpg` in any terminal or file
#       manager, so stripping only C0 would close the ANSI vector while leaving
#       the older extension-spoofing one wide open
#   Zl/Zp  line and paragraph separators, which break line-oriented consumers of
#       a file listing
#   Cs  lone surrogates, which cannot be encoded to UTF-8 at all
_STRIPPED_CATEGORIES = frozenset({"Cc", "Cf", "Zl", "Zp", "Cs"})


def _is_disallowed(ch: str) -> bool:
    return unicodedata.category(ch) in _STRIPPED_CATEGORIES

# Path separators and shell/Windows reserved characters become underscores.
_RESERVED = str.maketrans({ch: "_" for ch in '/\\:*?"<>|'})

_WHITESPACE_RUN = re.compile(r"\s+")

# ext4 accepts 255 bytes per component; 200 leaves room for the "{id}_" prefix
# and the extension, both of which are byte-counted too.
MAX_NAME_BYTES = 200
MAX_EXT_BYTES = 16


def export_root(out_dir: Path, peer_id: int) -> Path:
    """`<out>/g<chat_id>` - the directory name is the chat id, never the title.

    peer_id comes from telethon.utils.get_peer_id(entity). An int cannot
    traverse, cannot collapse into the parent when empty, cannot collide across
    groups, and cannot strand an in-progress export when an admin renames the
    group. The human-readable name is written to <root>/title.txt as data.

    The `g` prefix only avoids a leading `-` in shell arguments; it is ergonomic,
    not a security property.
    """
    _require_int(peer_id, "peer_id")
    return (Path(out_dir) / f"g{peer_id}").resolve()


def post_dir(root: Path, post_id: int) -> Path:
    """One directory per logical post, named for the lowest message id in it."""
    _require_int(post_id, "post_id")
    return root / str(post_id)


def _require_int(value: object, label: str) -> None:
    # bool is an int subclass, and a bool here always means a caller bug.
    if not isinstance(value, int) or isinstance(value, bool):
        raise TypeError(f"{label} must be an int, got {type(value).__name__}: {value!r}")


def derive_filename(msg) -> str:
    """`{message_id}_{sanitized stem}{ext}` - keyed on the message id.

    One Telegram message carries at most one media, so the message id makes
    uniqueness inside a post folder structural rather than positional.

    A positional index is unstable under history mutation: delete one album
    member between runs and every later index shifts, silently orphaning files
    already on disk. It would also force Post to carry the full pre-filter
    member list purely for naming, reintroducing the filter coupling that
    Invariant 1 exists to remove.

    The redundancy in `1042/1042_photo.jpg` buys self-identifying files and lets
    `grep 1043 messages.jsonl` join sidecar to disk with no index arithmetic.
    """
    _require_int(getattr(msg, "id", None), "msg.id")
    stem = sanitize(file_stem(msg), fallback=media_kind(msg) or "media")
    return f"{msg.id}_{stem}{sanitize_ext(file_ext(msg))}"


def sanitize_ext(ext: str) -> str:
    """The extension is part of the untrusted leaf and gets the same treatment.

    Sanitizing only the stem left a hole: the extension is derived from the same
    attacker-supplied DocumentAttributeFilename, so `holiday.<NUL>jpg` produced a
    name containing a NUL, and the ValueError that Path.resolve then raised
    escaped the download loop - halting the export on that message on every
    subsequent run, since the cursor could not advance past it. Control
    characters also meant an operator's terminal could be fed ANSI escapes from
    a group member's filename.
    """
    body = sanitize(str(ext).lstrip("."), fallback="bin", max_bytes=MAX_EXT_BYTES)
    return f".{body}"


def sanitize(name: str, *, fallback: str, max_bytes: int = MAX_NAME_BYTES) -> str:
    """Reduce an untrusted string to one safe path component.

    Order matters: control characters go before the leaf is taken, so a NUL
    cannot confuse the split; the leaf is taken before separators are replaced,
    so `../../../etc/passwd` loses its directories rather than becoming
    `.._.._.._etc_passwd`.
    """
    s = "".join(ch for ch in str(name) if not _is_disallowed(ch))
    s = _leaf(s)                              # discards every directory part, incl. ..
    s = s.translate(_RESERVED)
    s = _WHITESPACE_RUN.sub(" ", s).strip()
    s = _strip_edges(s)
    s = _truncate_utf8(s, max_bytes)
    s = _strip_edges(s)                       # truncation can expose a new leading dot
    return s or fallback


def _leaf(s: str) -> str:
    """The final component only. Both separators, so a Windows-style path passed
    to a Linux host still loses its directories."""
    return Path(s.replace("\\", "/")).name


def _strip_edges(s: str) -> str:
    """Leading dots hide files; leading dashes make a filename look like a flag
    to any tool the export is later piped through. Trailing dots and spaces are
    stripped because they survive on Linux and silently vanish elsewhere."""
    return s.lstrip(". -\t").rstrip(". \t")


def _truncate_utf8(s: str, max_bytes: int) -> str:
    """Truncate by UTF-8 bytes, preserving the extension.

    ext4's limit is 255 *bytes*, and a name of CJK or emoji characters overflows
    it well before 255 characters - a character-counted truncation raises
    OSError: File name too long partway through an export.
    """
    if len(s.encode()) <= max_bytes:
        return s
    stem, ext = os.path.splitext(s)
    ext_bytes = ext.encode()
    if len(ext_bytes) > max_bytes:            # pathological "extension"; drop it
        stem, ext, ext_bytes = s, "", b""
    budget = max_bytes - len(ext_bytes)
    # errors="ignore" discards a partial multi-byte character at the cut.
    return stem.encode()[:budget].decode("utf-8", "ignore") + ext


def safe_join(post_dir: Path, filename: str) -> Path:
    """Join and assert containment. Raises on escape - never silently repairs.

    sanitize should make this unreachable. Reaching it means sanitize has a hole,
    so repairing here would convert a discovered bug into a silent one.
    """
    base = Path(post_dir).resolve()
    resolved = (base / filename).resolve()
    if resolved == base or not resolved.is_relative_to(base):
        raise ValueError(f"path escape attempt: {filename!r} against {post_dir}")
    return resolved
