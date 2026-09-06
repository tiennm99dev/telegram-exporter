---
name: repo-exit-code-contract
description: tgexport's exit codes are a machine contract an external until-complete driver loops on; miscategorising one causes an infinite retry loop
metadata:
  type: project
---

`tgexport` exit codes: 0 complete, 1 ran but files remain, 2 usage, 3
remote/Telegram failure, 130/143 interrupted. Code 1 is the *retry* signal — an
external driver re-invokes on 1 and stops on 0 or 3.

**Why:** the retired `export-until-complete.sh` had two stop conditions the Go
rewrite deliberately dropped (`plans/.../phase-06-cli-resume-and-observability.md`
lines 46-57): a pass counter, and a no-progress detector. The Go tool converges
in one invocation instead, so the exit code is now the *only* thing that can
stop a driver.

**How to apply:** when reviewing any new error path in `cmd/tgexport`, ask which
exit code it produces and whether that state is actually retryable. Two states
have no representation in the contract and therefore fall through to 1 forever:
work that can never succeed (an unwritable filename, permanently unavailable
media) and a remote that is full or broken mid-run. Treat "exit 1 with no
possible progress" as a blocking defect, not a cosmetic one.

Related: [[rclone-embedding-traps]]
