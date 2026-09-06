# Agent Memory Index

- [rclone embedding traps](rclone-embedding-traps.md) — `NewFilter(nil)` is not neutral; `RCLONE_*` reaches globals at package init, so `t.Setenv` tests prove nothing.
- [Exit-code contract](repo-exit-code-contract.md) — exit 1 is the driver's retry signal; a non-retryable state that exits 1 loops forever.
