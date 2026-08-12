---
phase: 7
title: "End-to-End Validation"
status: pending
priority: P1
dependencies: [6]
effort: "~2h + export runtime"
---

# Phase 7: End-to-End Validation

## Overview

Prove the tool against a real group, with deliberate interruption. The failure modes that matter — cursor ahead of reality, split albums, flood-wait escalation — are invisible in unit tests and surface only against live data.

Every step below has a checkable outcome. **No step may pass by escape clause.**

## Requirements

- Functional: full export completes, resumes correctly after kill, produces a consistent sidecar
- Non-functional: README documents setup and semantics; no secret or third-party data is committable; the spike's server-side session is revoked

## Validation Script

1. **Dry run** — `tg-export --group <g> --out ./exports --dry-run`. Record posts, files, total bytes, verdict. Confirm no export root was created.

2. **Limited real run** — `--limit 20`. Inspect by hand: albums collapsed into one folder, standalone messages in their own, filenames `{message_id}_{name}`, `title.txt` written, directory named `g<chat_id>`.

3. **Sidecar cross-check** — scripted, not by eye:
   ```bash
   python3 - <<'EOF'
   # 1. every files[].path in messages.jsonl exists on disk
   # 2. every file on disk appears in the UNION of records for its message_id
   # 3. no two post folders share a grouped_id   <- catches album splitting
   EOF
   ```

4. **Idempotency** — `--reset-state`, then re-run `--limit 20`. Expect **zero downloads, all skips**.
   *(Re-running `--limit 20` without resetting would correctly continue to posts 21–40 — the cursor advanced because those 20 posts genuinely completed. Resetting is what isolates the dedupe path, which is the thing under test.)*

5. **Interruption** — start a full run, `kill -9` after ~50 files. Re-run. Confirm: no surviving `.part`, no re-downloads of completed files, no gaps, the in-flight album complete.

6. **Network loss** — drop the interface or block Telegram with iptables mid-download for ~30s, then restore. Confirm the transient path retries with backoff and the run continues. `kill -9` alone does not exercise exception propagation or reconnect, so it cannot stand in for this.

7. **Filter guard** — re-run with `--types video` against the existing state. Confirm it refuses, **names the changed filters**, and exits 7. Then `--reset-state` and confirm it proceeds and rotates `messages.jsonl`.

8. **Concurrency guard** — start a run; while it holds the lock, start a second against the same `--out`. Confirm exit 3 with pid/host/start-time, and that no file was touched. Confirm a run against a *different* group proceeds.

9. **Path safety** — covered by `tests/test_paths.py` and `tests/test_download_decisions.py`, which include the hostile-filename case offline. *(The previous "otherwise rely on unit tests and note no live sample existed" escape clause is deleted — it made passing the default.)* Optionally self-send a message named `../../../etc/passwd` to a scratch group for live confirmation.

10. **Full export** — run to completion. Confirm `completed_at` is set. Reconcile against step 1: post and file counts **exactly equal**; bytes within **2%** (photo approximation only). Any file-count delta is a bug to investigate, never accepted — expected differences are only deleted/expired media, which appear in `errors[]` and the exit summary.

11. **Flood-wait behavior** — grep logs: each wait slept through once, no retry storm, no restart during an active wait, and every entry carries a computed wake time. Separately confirm `--max-flood-wait 1` exits 6 cleanly with a resumable cursor, since that path is unreachable by default.

11b. **`--include-text`** — run with the flag on a scratch range. Confirm text-only messages appear with `"files":[]`, create no folders, and that toggling the flag against the existing cursor triggers the exit-7 refusal.

12. **Memory** — `/usr/bin/time -v tg-export --dry-run` → peak RSS under 200 MB. The album tripwire set is O(albums), not O(1); the criterion states the real bound.

13. **Revoke the spike session** — Telegram → Settings → Devices → terminate the phase 1 probe session. **Deleting `spike/probe.session` does not revoke server-side authorization**; a live auth key otherwise survives for an account you believe is clean.

## Related Code Files

- Modify: `README.md`
- Delete: `spike/` (or leave gitignored)

## README Must Document

- Obtaining `api_id`/`api_hash`; the credential policy table (env vs prompt vs getpass; what `.env` may contain)
- First-run login incl. 2FA; session path and why it defaults outside the repo
- **Why the Bot API cannot do this** — the first question any reader will have
- Resume semantics: the cursor means "handled through here *under these filters*", and why changing filters requires `--reset-state`
- Sidecar semantics: records are append-only events; **union over records per `message_id`**, not last-write-wins
- Export directory is `g<chat_id>` with the name in `title.txt`, and why (path safety + surviving group renames)
- Why downloads are sequential — flood-wait escalation, and that `--workers` deliberately does not exist
- `--include-text`: off by default; when set, text-only messages appear in the sidecar with `"files":[]` and no folder. It is part of the stored filter set, so toggling it requires `--reset-state`
- **Flood waits have no ceiling by default** — the tool sleeps as long as Telegram demands, logging the computed wake time each time. A long sleep is a sleep, not a hang; check the log before assuming otherwise. Pass `--max-flood-wait SECONDS` to exit resumably instead

<!-- Updated: Validation Session 1 - --include-text docs; flood-wait default inverted -->

- That the dry-run verdict is **advisory**; the per-file disk check enforces. A `SHORT` verdict means the run stops partway, resumably — not that it is unsafe
- `cryptg` is optional and not installed by default: native code in Telethon's AES path, trusted (if adopted) because the Telethon author maintains it — not because it is "CPU-side only"

## Success Criteria

- [ ] Every step 1–13, including 11b, passes with its stated outcome — no step passes by escape clause
- [ ] Full export completes with `completed_at` set; counts reconcile within the stated tolerance
- [ ] Kill-and-resume produces a complete export with no gaps and no duplicates
- [ ] Network-loss interruption recovers without operator intervention
- [ ] Filter guard fires with a named diff and clears with `--reset-state`
- [ ] Concurrency guard exits 3; a different group is unaffected
- [ ] `pytest` green; `pip install -r requirements.txt -e .` from a clean venv yields a working `tg-export`
- [ ] `git status` clean of session files, exports, state, lock, and `.env`
- [ ] Spike session terminated server-side
- [ ] README covers every item listed above

## Risk Assessment

- **Group larger than free disk** → step 1 reports it; the per-file guard stops cleanly and resumably. Scope down with `--max-size`/`--types` or export to external storage.
- **Long export interrupted by network loss** → step 6 is the proof; step 5 alone would not be.
- **Estimate/actual discrepancy** → expected sources are deleted/expired media (visible in `errors[]`) and photo approximation. Anything else is a filter or traversal bug — investigate.
- **Album split** → step 3's grouped_id check plus phase 3's runtime tripwire. A split that reaches disk is silent otherwise.
- **Account flood-limited during validation** → keep `--limit` small in steps 2–8; run step 10 once, unattended.
- **Third-party data committed** → the sidecar contains message text and sender names. Export root is gitignored and `assert_not_committable` refuses an un-ignored path inside the repo; verify before any commit.
- **Spike credential left live** → step 13. The most-missed cleanup item, because the local file looks like the whole credential.
