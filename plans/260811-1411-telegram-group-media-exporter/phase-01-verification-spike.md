---
phase: 1
title: "Verification Spike"
status: partial
priority: P1
dependencies: []
effort: "~45 min"
---

# Phase 1: Verification Spike

## Overview

Throwaway script settling the unverified items in the research confidence ledger against the live API, before production code depends on them. The docs page truncated mid-signature; guessing means silent resume bugs discovered after a 40 GB run.

**Ordering is load-bearing in this phase.** `.gitignore` is committed *first*, before anything can create a credential. The spike uses its own session path so phase 2's "first production login" remains a real, verifiable event.

## Requirements

- Functional: answer all six probes; record findings; amend contradicted phases
- Non-functional: throwaway code, not shipped; no credential exists before `.gitignore` is committed; one test download only

## Probes

### P1 — `offset_id` direction under `reverse=True` (highest risk)

Semantics invert between sweep directions. Wrong here means resume silently skips or re-downloads everything.

```python
ids = [m.id async for m in client.iter_messages(entity, limit=5)]   # newest-first
mid = sorted(ids)[2]
fwd = [m.id async for m in client.iter_messages(entity, reverse=True, offset_id=mid, limit=3)]
assert all(i > mid for i in fwd), f"offset_id not exclusive-lower-bound under reverse: {fwd} vs {mid}"
```

Record whether `mid` itself is included. **Probe `min_id=mid` the same way** — it is documented as an exclusive lower bound and may be the more robust parameter regardless. The result decides whether the cursor stores "last handled id" or "next id to fetch".

### P2 — 2FA / login flow

Record whether `SessionPasswordNeededError` is raised and whether `client.start(phone=..., password=<callable>)` handles it. Confirm a second run against the same session needs no interaction.

2FA password is `getpass`-only — never argv, never env, never a file. (This supersedes any earlier wording permitting env.)

### P3 — `message.file` attribute surface

**Open question, not a confirmation.** For one photo, one video, one document, one voice note, one text-only message, and anything unusual in reach (poll, webpage preview, expiring media):

```python
f = msg.file
print(msg.id, msg.grouped_id, type(msg.media).__name__,
      getattr(f, 'name', None), getattr(f, 'ext', None),
      getattr(f, 'size', None), getattr(f, 'mime_type', None))
```

Answer: **for which media kinds is `.size` absent?** Do not assume it is always present — phases 3, 5, and 6 each need a rule for `None`.

### P4 — `download_media` return value and file-object support

- Download one photo and one document with `file=<path>`; record the exact return type/value. Phase 5 uses this as the source of truth for `os.replace`.
- Test whether `download_media(msg, file=<open file object>)` works across media branches. Phase 5 needs to own the fd to `fsync` it before rename. If it does not work, the fallback is download-to-path then re-open and fsync.

### P5 — photo `.size` vs bytes on disk (NEW — ledger item the original probe set missed)

```python
p = await client.download_media(photo_msg, file=str(tmp))
print("declared", photo_msg.file.size, "actual", os.path.getsize(p),
      "delta", os.path.getsize(p) - photo_msg.file.size)
```

Phase 6 documents photo sizes as approximate while phase 5 originally used exact equality as a hard gate — that contradiction was the poison pill this probe closes. The resolved design is answer-independent; the answer only improves report labelling and validator tuning.

### P6 — album adjacency across a pagination boundary

The original probe capped `limit ≤ 5`, which cannot cross Telethon's ~100-message page boundary and so could not test the thing it existed to test. **Cap downloads at 1; do not cap history reads.**

```python
rows = [(m.id, m.grouped_id) async for m in
        client.iter_messages(entity, reverse=True, offset_id=start, limit=350)]
assert len(rows) >= 250, "window too small to cross a page boundary — probe is vacuous"
ids = [i for i, _ in rows]
assert ids == sorted(ids), "sweep is not strictly ascending"
runs = [k for k, _ in itertools.groupby(rows, key=lambda r: r[1])]
gids = [g for g in runs if g is not None]
assert len(gids) == len(set(gids)), f"a grouped_id occupies >1 run — albums DO split: {gids}"
```

Coverage is probabilistic (3+ pages, several albums). The deterministic guarantee is phase 3's runtime tripwire; this probe is the cheap prior.

## Related Code Files

- Create: `.gitignore` (final version, committed first), `requirements.txt`
- Create: `spike/probe_telethon_api.py` (throwaway, gitignored)
- Create: `plans/reports/spike-260811-1411-telethon-api-verification-report.md`

## Implementation Steps

1. Write the **final** `.gitignore` and commit it. Nothing else exists yet:
   ```
   .venv/
   *.session
   *.session-journal
   *.session-wal
   *.session-shm
   spike/
   exports/
   .export-state.json
   .export.lock
   __pycache__/
   *.egg-info/
   .env
   ```
2. **Gate — must pass before step 3:** `git check-ignore -q .venv spike/ && echo OK` (exit 0)
3. `python3 -m venv .venv && .venv/bin/pip install "telethon>=1.41,<2"`, then `.venv/bin/pip freeze > requirements.txt` — this records the exact version the probes verified
4. `umask 077; mkdir spike`
5. Obtain `api_id`/`api_hash` from my.telegram.org; export as `TG_API_ID`/`TG_API_HASH` (never hardcode)
6. Write and run `spike/probe_telethon_api.py` (P1–P6), session at `spike/probe.session`
7. Write the findings report, marking each ledger item CONFIRMED or CORRECTED, and recording `telethon.__version__`
8. **If any finding contradicts the plan, amend the affected phase file before proceeding**

## Success Criteria

- [ ] `.gitignore` is committed before any venv, session, or credential exists — verified by `git log`
- [ ] `git check-ignore -q spike/ .venv` exits 0 before step 3 runs
- [ ] P1: `offset_id` inclusivity/direction under `reverse=True` recorded, and `min_id` compared
- [ ] P2: 2FA flow documented; second run confirmed non-interactive
- [ ] P3: `.size` availability tabulated per media kind, including any kind where it is absent
- [ ] P4: `download_media` return value recorded; file-object support determined
- [ ] P5: photo declared-vs-actual size delta recorded
- [ ] P6: ≥250 messages swept, ascending order asserted, no `grouped_id` occupying two runs
- [ ] `requirements.txt` records the verified Telethon version
- [ ] Findings report written; contradicted phases amended

## Risk Assessment

- **Wrong `offset_id` assumption reaches production** → the point of this phase; assertions must run, not be eyeballed.
- **Credential created before `.gitignore`** → step 1 precedes step 3, with an explicit gate between them.
- **Probe capped too small to test what it claims** → P6 requires ≥250 messages and asserts it. History reads are cheap (~4 requests); only *downloads* are meaningfully rate-limited.
- **Spike session left live** → deleting `spike/probe.session` does **not** revoke server-side authorization. Phase 7 cleanup must terminate it in Telegram → Settings → Devices.
- **Credentials leaked into the spike script** → env vars only; `spike/` gitignored from commit one.
