---
phase: 4
title: "Naming and Path Safety"
status: complete
priority: P1
dependencies: [2]
effort: "~1h"
---

# Phase 4: Naming and Path Safety

## Overview

`paths.py` — **all** path construction, from the export root down to the filename leaf. Pure functions, no network, fully unit-testable, which is why it is its own module rather than a helper inside `downloader.py`.

Renamed from `naming.py`: the module owns directory construction too. That rename is what turns the group-title fix from a patch into a structural property.

## Requirements

- Functional: derive the export root, post dir, and filename; sanitize; guarantee uniqueness within a post folder
- Non-functional: pure; deterministic across runs *and across filter sets* (dedupe depends on it)

## The governing invariant

> **Exactly one untrusted string ever becomes a path component: the filename leaf.**

Every other component is an `int`, enforced by a type check. This makes `safe_join`'s "post_dir is trusted" precondition true *by type* rather than by assumption.

Two untrusted strings were previously in play:

1. `DocumentAttributeFilename` — attacker-controlled, supplied by arbitrary group members. `../../../.ssh/authorized_keys` is a cheap, real attack. Always was in scope.
2. **The group title** — server-supplied and editable by any group admin, used to build `exports/<group-slug>/`. This was *not* in scope, and `safe_join` could never have caught it because the escape happens in the parent component, before the join.

## Architecture

```python
def export_root(out_dir: Path, peer_id: int) -> Path:
    """peer_id from telethon.utils.get_peer_id(entity). int -> traversal impossible."""
    if not isinstance(peer_id, int):
        raise TypeError(peer_id)
    return (out_dir / f"g{peer_id}").resolve()        # g-1001234567890

def post_dir(root: Path, post_id: int) -> Path:
    if not isinstance(post_id, int):
        raise TypeError(post_id)
    return root / str(post_id)

def derive_filename(msg) -> str:                       # note: no index parameter
    return f"{msg.id}_{sanitize(stem_for(msg), fallback=kind(msg))}{ext_for(msg)}"

def sanitize(name: str, *, fallback: str, max_bytes: int = 200) -> str:
    """Reduce an untrusted string to a safe single path component."""

def safe_join(post_dir: Path, filename: str) -> Path:
    """Join and assert containment. Raises on escape — never silently repairs."""
```

### Why the directory is the chat id, not the title

The `g` prefix avoids a leading `-` (a CLI foot-gun, not a security issue). The human-readable name is written once to `<root>/title.txt` and printed in the run header — stored as data, never as a path.

Rejected alternatives: a sanitized title slug needs a second sanitizer with different rules for directories, a fallback for the empty case, and a collision story — more code and more risk. A `<slug>-<chat_id>` hybrid is readable but strands the state file and the downloaded tree in the old directory when the group is renamed mid-export.

One move fixes traversal, empty-slug collapse into the parent, cross-group collision, *and* rename-orphaning — and it is a net code deletion. The cost is that `ls exports/` shows numbers.

### Why filenames key on `message_id`, not a positional index

One Telegram message carries at most one media, so `message_id` cannot collide inside a post folder. Uniqueness becomes structural rather than positional.

A positional index — even one computed pre-filter — is unstable under *history mutation*: if a member message is deleted between runs the group shrinks and every later index shifts, silently orphaning already-downloaded files. It would also force `Post` to carry the full pre-filter member list purely for naming, reintroducing the coupling Invariant 1 exists to remove.

Accepted cost: `1042/1042_photo.jpg` is redundant for standalone posts. Worth it — files stay self-identifying when moved out of their folder, and `grep 1043 messages.jsonl` joins sidecar to disk with no index arithmetic.

### Sanitization rules

Derivation order: `msg.file.name` when present → `msg.file.ext`/mime with a kind-based stem (`photo`, `video`, `voice` carry no filename) → `.bin`.

- Take `Path(name).name` only — discards every directory component, including `..`
- Strip NUL and control chars (`< 0x20`); replace path separators and reserved chars
- Strip leading dots and dashes (hidden files, argument-looking names)
- Collapse whitespace runs
- Truncate to `max_bytes` counted in **UTF-8 bytes, not characters** — ext4's limit is 255 bytes and unicode names overflow well before 255 chars. Preserve the extension.
- Empty or fully-stripped result → `fallback`

Final defense in `safe_join`:

```python
resolved = (post_dir / filename).resolve()
if not resolved.is_relative_to(post_dir.resolve()):
    raise ValueError(f"path escape attempt: {filename!r}")
```

Sanitization should make this unreachable; reaching it means sanitization has a hole — hence raise, never repair.

## Related Code Files

- Create: `src/telegram_exporter/paths.py`
- Create: `tests/test_paths.py`

## Implementation Steps

1. Implement `sanitize` per the rules above
2. Implement `derive_filename` using phase 1 P3's findings on `message.file` availability
3. Implement `export_root` / `post_dir` with `isinstance` guards
4. Implement `safe_join` with the containment assertion
5. Write the table-driven tests below
6. `pytest tests/test_paths.py` must pass before phase 5 consumes this module

## Test Table

| Input | Expected |
|---|---|
| `../../../etc/passwd` | `passwd` |
| `a/b/c.jpg` | `c.jpg` |
| `..` / `.` / `""` | fallback |
| `\x00evil.jpg` | `evil.jpg` |
| `....jpg` | leading dots stripped |
| `-rf.jpg` | leading dash stripped |
| 300-char unicode stem | ≤200 UTF-8 bytes, extension intact |
| same msg, two different filter sets | **identical** filename |
| two media in one post | distinct names via distinct message ids |
| `safe_join(dir, "../x")` | raises `ValueError` |
| `export_root(out, "..")` | raises `TypeError` (not an int) |
| `export_root(out, "../../etc")` | raises `TypeError` |
| `post_dir(root, "1042; rm -rf")` | raises `TypeError` |

## Success Criteria

- [ ] Every row of the test table passes
- [ ] No sanitized name, joined to a post dir, resolves outside it
- [ ] `export_root` and `post_dir` reject every non-`int` input
- [ ] A group titled `..`, `a/b`, or `""` cannot influence the output path — there is no code path from title to path
- [ ] Truncation is byte-counted; a 300-char unicode name yields a filename ext4 accepts, extension intact
- [ ] `derive_filename` is deterministic across runs **and across filter sets**
- [ ] Tests run offline with no Telethon client

## Risk Assessment

- **Path traversal via filename** → sanitize + `safe_join` assertion + table tests.
- **Path traversal via group title** → structurally impossible: no code path exists from title to path component.
- **Filter-dependent filenames** → keyed on `message_id`; asserted by the two-filter-sets test row.
- **Index instability under message deletion** → no positional index exists.
- **Char-based truncation on unicode** → `OSError: File name too long` mid-export. Byte-counted.
- **Non-deterministic fallbacks** (e.g. a timestamp) → dedupe breaks and every re-run re-downloads. Fallbacks derive only from message id and kind.
- **Windows reserved names** (`CON`, `PRN`) → not applicable on Linux; deliberately skipped.
