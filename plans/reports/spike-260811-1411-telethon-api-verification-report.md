---
title: "Phase 1 — Telethon API Verification Findings"
phase: 1
status: partial
telethon_version: "1.44.0"
created: "2026-08-12"
---

# Phase 1 — Telethon API Verification Findings

Two sources of evidence appear below and are labelled separately:

- **SOURCE** — settled offline by reading the installed `telethon` 1.44.0 package.
  Deterministic, and stronger than a single live observation for questions about
  parameter semantics, because it shows the code path rather than one sample of
  its behavior.
- **LIVE** — requires `TG_API_ID`/`TG_API_HASH`, an interactive login, and a real
  group. **Not yet run.** Run `spike/probe_telethon_api.py` and paste its output
  into the corresponding section.

Environment: Python 3.12.3, aarch64, Telethon **1.44.0** (wheel, no build step),
pinned in `requirements.txt`.

## Confidence ledger

| Probe | Question | Status | Answer |
|---|---|---|---|
| P1 | `offset_id` direction/inclusivity under `reverse=True` | **CONFIRMED (SOURCE)** | Exclusive lower bound: results satisfy `id > offset_id` |
| P2 | 2FA / login flow | **PENDING (LIVE)** | — |
| P3 | `message.file` surface; which kinds lack `.size` | **PENDING (LIVE)** | — |
| P4 | `download_media` return value; file-object support | **PARTIAL (SOURCE)** | File objects are accepted; return value pending live confirmation |
| P5 | photo declared `.size` vs bytes on disk | **PENDING (LIVE)** | — |
| P6 | album adjacency across a pagination boundary | **PENDING (LIVE)** | Runtime tripwire implemented regardless |
| — | error classes in phase 5's taxonomy exist | **CONFIRMED (SOURCE)** | All 15 present |

## P1 — `offset_id` under `reverse=True` — CONFIRMED (SOURCE)

`telethon/client/messages.py`, `_MessagesIter._init`:

```python
if self.reverse:
    if offset_id:
        offset_id += 1
    elif not offset_date:
        offset_id = 1
```

and `_update_offset`, run after every page:

```python
self.request.offset_id = last_message.id
if self.reverse:
    self.request.offset_id += 1   # "We want to skip the one we already have"
```

**`offset_id` is an exclusive lower bound under `reverse=True`.** `offset_id=N`
yields messages with `id > N`; `N` itself is excluded. `offset_id=0` starts from
the beginning of history.

`min_id` was compared as the plan asked: `_message_in_range`'s `reverse` branch
tests `message.id <= self.last_id or message.id >= self.max_id` and **does not
consult `min_id` at all**, so `offset_id` is the correct parameter for a
chronological sweep, not `min_id`.

**Consequence, implemented:** the cursor stores the **last handled id** and is
passed as `offset_id`. `iter_posts` additionally carries an explicit
`if msg.id <= after_id: continue` guard, so the sweep stays correct even if this
internal detail changes in a future Telethon release —
`src/telegram_exporter/traversal.py`.

## Error-class inventory — CONFIRMED (SOURCE)

Every exception phase 5's taxonomy catches exists in 1.44.0 (checked against
`telethon.errors` and `telethon.errors.rpcerrorlist`). A missing name would have
been an import-time crash on the first run:

`FloodWaitError`, `FileReferenceExpiredError`, `MediaEmptyError`,
`FileIdInvalidError`, `FilerefUpgradeNeededError`, `AuthKeyUnregisteredError`,
`AuthKeyDuplicatedError`, `SessionRevokedError`, `UserDeactivatedBanError`,
`ChannelPrivateError`, `ChatForbiddenError`, `ChannelInvalidError`,
`TimedOutError`, `RpcCallFailError`, `SessionPasswordNeededError`.

Constructor signature for all of them is `(request)`, with `FloodWaitError`
taking `(request, capture=0)` where `capture` becomes `.seconds`. The test suite
constructs them this way.

## P4 — `download_media` file-object support — PARTIAL (SOURCE)

`Message.file` wraps `photo or document` and is `None` for other media types
(polls, games, none), which is why `media.media_kind()` classifies from the media
type rather than from `.file`.

Telethon accepts a stream for `file=`, so phase 5 holds the fd and `fsync`s it
before `os.replace`. The plan's fallback — download to a path, then re-open and
fsync — is not needed unless the live probe contradicts this.

**Still pending (LIVE):** the exact return type/value of
`download_media(msg, file=<path>)`.

## P3 / P5 note — why the implementation does not wait on these

Both remaining questions are about *sizes*, and the resolved design is
answer-independent by construction:

- `media.media_size()` returns `int | None`, and each of the four consumers has a
  stated rule for `None` (include+warn / accept / separate tally / treat as 0).
  Tested in `tests/test_traversal.py`.
- The download validator never raises: a photo mismatch is accepted as a size
  variant, and a document/video/audio mismatch retries then reports FAILED.
  Tested in `tests/test_download_decisions.py`.

P3's answer improves the `--max-size` warning's accuracy; P5's improves the
dry-run label and phase 7's reconciliation tolerance. Neither can invalidate code
that already treats `None` as a first-class case.

## P2 / P6 — PENDING (LIVE)

- **P2** — record whether `SessionPasswordNeededError` is raised, and confirm a
  second run is non-interactive. The 2FA password is `getpass`-only, enforced by
  `tests/test_logging.py`.
- **P6** — the probe requires ≥250 messages swept and asserts it rather than
  passing vacuously. Coverage is probabilistic either way; the deterministic
  guarantee is the runtime tripwire in `traversal._close`, which raises
  `AlbumSplitError` when a `grouped_id` reopens after its run closed. Tested for
  both `A B A` and `A B C A`.

## Cleanup owed after running the spike

Deleting `spike/probe.session` does **not** revoke server-side authorization.
Terminate the probe session in Telegram → Settings → Devices. This is phase 7
step 13 and the most-missed item, because the local file looks like the whole
credential.

## Unresolved

1. P2, P3, P5 and P6 need a live run — no Telegram credentials exist in this
   environment.
2. If P3 finds a media kind whose `.size` is absent *and* commonly large, revisit
   only the estimate's `unknown size` line; no code change is implied.
