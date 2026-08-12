---
phase: 3
title: "Traversal Grouping and Filters"
status: complete
priority: P1
dependencies: [2]
effort: "~2h"
---

# Phase 3: Traversal Grouping and Filters

## Overview

The single traversal generator both run modes consume: chronological sweep, album buffering, filter application. Yields `Post` objects — one per logical post a human would recognize.

## Requirements

- Functional: sweep entire history oldest→newest; group album members; apply filters; support resume from a cursor; never stall the cursor across filtered regions
- Non-functional: bounded memory; no downloads; an album split converts to a loud failure, never silent corruption

## Architecture

```python
@dataclass(frozen=True)
class Post:
    post_id: int              # lowest message id in the FULL group (pre-filter)
    messages: list[Message]   # filter-surviving members, ascending by id
    max_message_id: int       # highest id in the FULL group — the cursor value
```

`grouped_id` is deliberately **not** a field — the sidecar reads it off the message, and folder identity uses `post_id`. Nothing else consumed it.

**Invariant 1 — identity is filter-invariant.** `post_id` and `max_message_id` derive from the complete `grouped_id` group before filters drop members. Phase 4's filenames key on `message_id` for the same reason, so the file index no longer depends on the filter set either.

### `iter_posts` — always yields, even when empty

```python
async def iter_posts(client, entity, *, after_id=0, filters, limit=None):
    buf, closed, prev = [], set(), 0
    agen = lambda since: client.iter_messages(entity, reverse=True, offset_id=since)
    async for msg in aiter_with_flood_retry(agen, start_id=after_id):
        assert msg.id > prev, f"sweep not strictly ascending: {msg.id} after {prev}"
        prev = msg.id
        if buf and (msg.grouped_id is None or msg.grouped_id != buf[0].grouped_id):
            yield _close(buf, closed, filters); buf = []
        if msg.grouped_id is None:
            yield _close([msg], closed, filters)
        else:
            buf.append(msg)
    if buf:
        yield _close(buf, closed, filters)

def _close(buf, closed, filters) -> Post:
    gid = buf[0].grouped_id
    if gid is not None:
        if gid in closed:                       # album-split tripwire
            raise AlbumSplitError(
                f"grouped_id {gid} reopened at msg {buf[0].id} after being closed — "
                f"album split across a non-adjacent boundary. Do not trust this export.")
        closed.add(gid)
    return Post(post_id=buf[0].id,
                messages=[m for m in buf if _keep(m, filters)],
                max_message_id=buf[-1].id)
```

**Empty posts are yielded, not skipped.** This is the resolved answer to a decision the plan previously deferred. Skipping them stalls the cursor: a `--types video` run over a photo-heavy tail would advance `cursor_id` only at videos, so every resume re-sweeps tens of thousands of messages to download nothing. The "phantom post count" objection is a *counting* bug, fixed with one line in each consumer (`if not post.messages: continue` before counting), not a design objection.

**Album-split tripwire.** Consecutiveness of `grouped_id` members in a sweep is assumed by the whole design and was never verified by any source. The tripwire converts a silent split — two folders for one album, no exception, no size mismatch, dedupe reporting complete — into a loud failure. Memory is one int per album (~6 MB at 100k albums); the "flat memory" criterion is restated honestly as O(albums) rather than pretending it is O(1).

Tracking only `last_flushed_gid` was rejected: it catches `A B A` but misses `A B C A`, saving 6 MB and losing most of the coverage.

### `media_size` — one rule, three consumers (DRY)

```python
def media_size(msg) -> int | None:
    """Single source of truth. Phase 1 P3 determines which kinds return None."""
```

| Consumer | Rule for `None` |
|---|---|
| `--max-size` filter | `size is None or size <= limit` → **include**, WARN once. Silent drops violate the no-silent-gaps ethos; unknown-size media is pathological and near-always small. |
| download validator | accept (nothing to check) |
| estimator | **not** `or 0` — counted in a separate `unknown size: N files (not counted)` line. An unbounded estimate error must be visible. |
| disk guard | treated as 0, absorbed by the reserve |

Comparing `None <= limit` unguarded raises `TypeError` inside the generator and kills a 20-hour export — that is what this helper exists to prevent.

### Filters

| Flag | Predicate |
|---|---|
| `--types` | media kind ∈ set; kind from `msg.media` type + document attributes |
| `--since` / `--until` | `msg.date` in range, UTC-aware on both sides |
| `--max-size` | via `media_size` per the table above |
| `--include-text` | when set, keep text-only messages too (sidecar context); default off |

<!-- Updated: Validation Session 1 - added --include-text -->

**`--include-text`** (validation decision). Default off: messages with no media are dropped. When set, text-only messages survive `_keep` and reach the sidecar with an empty `files[]`, so captions and replies have surrounding conversation. They never create a post folder and never reach `download_one`.

It **is** part of the stored filter set — it changes what a post contains, so running without it and then with it against the same cursor would silently skip every text record.

`--limit N` counts **posts with files** (counting all posts would let it terminate instantly inside a filtered region, and would now also be skewed by text-only posts) and is *not* a filter — it changes when we stop, not what a post contains, so it stays out of the state file's filter set.

## Related Code Files

- Create: `src/telegram_exporter/traversal.py`
- Create: `tests/test_traversal.py`
- Modify: `src/telegram_exporter/cli.py` (wire `--types`, `--since`, `--until`, `--max-size`, `--limit`)

## Implementation Steps

1. Define `Post`, `Filters`, `AlbumSplitError`; parse CLI values into `Filters` (size-string and ISO-date parsers)
2. Implement media-kind classification and `media_size`
3. Implement `iter_posts` per the sketch, honoring phase 1 P1's confirmed `offset_id`/`min_id` semantics
4. Consume the sweep through `aiter_with_flood_retry` — not the single-value wrapper
5. Unit test with **synthetic message stubs** (no network): objects with `id`, `grouped_id`, `date`, `file`
6. Test the tripwire explicitly: a synthetic stream where a `grouped_id` reappears must raise

## Success Criteria

- [ ] A 3-photo album yields exactly one `Post` with 3 messages
- [ ] Two adjacent albums yield two distinct `Post`s, not one merged
- [ ] A standalone message between two albums yields its own `Post`
- [ ] An album at the very end of history is flushed
- [ ] `post_id` and `max_message_id` are unchanged when a filter removes some members — asserted
- [ ] A fully-filtered group still yields a `Post` (with `messages == []`) so the cursor advances
- [ ] Text-only messages contribute no files, and are dropped entirely unless `--include-text` is set
- [ ] With `--include-text`, a text-only message survives into `Post.messages` but creates no folder and no download
- [ ] `--limit N` counts posts with files, unaffected by `--include-text`
- [ ] A synthetic stream reopening a closed `grouped_id` raises `AlbumSplitError`
- [ ] A non-ascending synthetic stream trips the assertion
- [ ] `media_size` returning `None` does not raise in any of the three consumers
- [ ] Filters compose: `--types video --since 2026-01-01 --max-size 50MB`
- [ ] `--limit 5` stops after 5 posts *with files*
- [ ] Album buffer never exceeds 20 entries (asserted in code)

## Risk Assessment

- **Identity computed post-filter** → duplicate folders and re-downloads across filter sets. Invariant 1, plus phase 4's message-id filenames; both explicitly tested.
- **Album split across a pagination boundary** → silent two-folder corruption. Phase 1 P6 is the prior; the tripwire is the guarantee. Phase 3's earlier dismissal of this risk was an unsourced assertion guarded by a test that structurally could not fail.
- **Cursor stalls across filtered regions** → empty posts are yielded. Decided here, not deferred to implementation.
- **`None` size crashes the sweep** → `media_size` plus the stated rule per consumer.
- **Flood wait mid-sweep** → `aiter_with_flood_retry` resumes from the last yielded id.
- **Timezone-naive comparison** → `msg.date` is UTC-aware; parse `--since`/`--until` as UTC-aware or the comparison raises.
