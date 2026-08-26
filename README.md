# telegram-exporter

Export a Telegram chat's media to **any rclone remote** — S3, Google Drive,
Dropbox, Backblaze B2, SFTP, WebDAV, or anything else rclone supports — using
far less local disk than the chat's total size.

`run.sh` runs [tdl](https://github.com/iyear/tdl) and
[rclone](https://rclone.org/) as a rolling pipeline: tdl downloads into a small
staging directory while rclone concurrently moves finished files to the remote
and deletes the local copies. Local disk only ever holds the files in flight
plus one sync interval of throughput, so a multi-terabyte chat exports fine on a
small disk. Telegram caps a single file at 2 GB (4 GB from premium uploaders),
so a few dozen GB of staging covers the worst case regardless of chat size.

This is needed because **tdl can only write to a local directory** — it has no
rclone integration and no remote destination of any kind (`tdl dl -d` takes a
filesystem path; `tdl --storage` is its session database, not an output target).
tdl downloads over MTProto with a user account, so Bot API limits do not apply:
full history is readable and there is no 20 MB download cap.

## Requirements

- **tdl** — <https://docs.iyear.me/tdl/getting-started/installation/>
- **rclone** — <https://rclone.org/install/>
- **bash**. On Windows, run under WSL or Git Bash.

## Setup

Both steps are one-time.

```bash
# 1) log in to Telegram with your user account (phone + code + 2FA)
tdl login

# 2) configure the destination. The interactive wizard covers every backend:
rclone config

# ...or create one non-interactively, e.g.
rclone config create gdrive drive
rclone config create b2 b2 account=KEY_ID key=APP_KEY
rclone config create dav webdav url=https://dav.example.com/remote.php/dav/files/you \
  vendor=other user=YOU pass=SECRET

rclone listremotes          # confirm the name you will pass to -r
```

Any rclone remote form works, including on-the-fly connection strings
(`:webdav,url=https://...:/path`).

## Usage

```bash
./run.sh -r gdrive:telegram/media -c @mygroup
```

That is the whole flow. It exports the chat's message metadata to
`export.json`, then downloads and uploads concurrently until finished. Progress
and warnings go to stderr; press Ctrl-C at any point and it stops cleanly.

### Options

| Flag | Meaning |
|------|---------|
| `-r REMOTE:PATH` | **Required.** rclone destination, e.g. `gdrive:telegram/media`, `s3:bucket/tg`, `dav:tg-export` |
| `-c CHAT` | Chat to export when the JSON does not exist yet — id, username, or link (see below) |
| `-f FILE` | Export JSON to download from (default `export-<chat>.json` with `-c`, else `export.json`) |
| `-d DIR` | Staging directory (default `./staging`) |
| `-i SECONDS` | Seconds between rclone sweeps (default `60`) |
| `-a AGE` | rclone `--min-age`, a second guard against moving files still being written (default `2m`) |
| `-h` | Help |

### Identifying the chat

`-c` accepts every form tdl understands, plus one it doesn't:

| Form | Example |
|------|---------|
| Numeric id, as printed by `tdl chat ls` | `-c 1697797156` |
| Username, with or without `@` | `-c @mygroup` / `-c mygroup` |
| Public link | `-c https://t.me/mygroup` / `-c t.me/mygroup` |
| Deep link | `-c 'tg://resolve?domain=mygroup'` |
| **Bot API id** (converted for you) | `-c -1001697797156` → `1697797156` |

tdl resolves a numeric argument as an MTProto id and anything else through
gotd's resolver. MTProto has no `-100` prefix, so a Bot API id would otherwise
fail to resolve; the script strips it and logs the conversion.

A **message** link is rejected — `-c` names a chat, not a message:

```
$ ./run.sh -r gdrive:tg -c https://t.me/mygroup/123
error: -c takes a chat, not a message link — pass the chat's username or id
```

Run `tdl chat ls` to see ids and usernames side by side.

Each chat gets its own export file by default (`export-mygroup.json`,
`export-1697797156.json`), so exporting a second chat from the same directory
never reuses the first one's JSON. When the file already exists it is reused and
the script says so — delete it to re-export.

Anything after `--` is passed straight to `tdl dl`:

```bash
./run.sh -r gdrive:telegram/media -- -t 4 -l 1      # calmer parallelism, fewer flood waits
./run.sh -r gdrive:telegram/media -- -i mp4,mkv     # only these file extensions
./run.sh -r gdrive:telegram/media -- -e jpg,png     # skip these file extensions
```

tdl defaults to `-t 8 -l 4`, which is aggressive; lower it if you hit flood
waits on a large export.

### Tuning the upload

rclone reads every one of its flags from an environment variable, so the upload
side is tunable without touching the script:

```bash
RCLONE_TRANSFERS=8 RCLONE_BWLIMIT=20M ./run.sh -r s3:bucket/tg -c @mygroup
```

### Exporting a subset

Generate the JSON yourself when you want a narrower export, then point `-f` at
it:

```bash
tdl chat export -c @mygroup -T id -i 1000,5000 --all --with-content -o part.json
./run.sh -r gdrive:telegram/media -f part.json
```

`tdl chat export` takes `-T time|id|last` with `-i` as the range, and `-f` as an
expression filter over message fields (`-f -` lists the available fields).

## Resuming

Re-run the same command. Both legs resume independently and nothing is
downloaded or uploaded twice.

Keep the same `export.json` between runs: `--skip-same` compares against the
**staging** directory, which is empty once files have moved to the remote, so
cross-run deduplication rests on tdl's own `--continue` tracking. If you must
start from a fresh export, narrow it to the missing message-id range
(`-T id -i <last>,<max>`) rather than re-downloading everything.

## What it guards against

- **Partial uploads.** tdl writes `<name>.tmp` and renames on completion, so
  every sweep excludes `*.tmp`. Age alone is not a completion signal: a download
  stalled by a flood wait stops touching its `.tmp`, which would then be
  uploaded half-written and lose its resume point.
- **Directories vanishing under tdl.** `--delete-empty-src-dirs` runs only in
  the final sweep, and the staging directory is recreated after every sweep.
  Removing a directory under a running tdl makes it fail to create its next file.
- **Orphaned downloads.** tdl is stopped on exit, Ctrl-C, or `SIGTERM`, so no
  download keeps running after the script is gone.
- **A failed run looking finished.** The unrestricted final sweep happens only
  after tdl exits 0. An interrupted or crashed run gets the age-guarded sweep
  and keeps staging for the next attempt.
- **A dead remote filling the disk.** Five consecutive rclone failures abort the
  run instead of letting staging grow unbounded.
- **Typos and bad credentials.** Before downloading anything, the remote must be
  present in `rclone listremotes` (skipped for connection strings) and the
  destination must be creatable, which proves both reachability and auth.

Exit codes: `0` success, `2` usage error, `3` rclone failure, `130`/`143`
interrupted, anything else is tdl's own exit code.

## Limits

- Streaming with no staging at all (piping download chunks straight to the
  remote) is not possible with tdl and would require custom code.
- The script is bash; the two tools it drives are cross-platform, but Windows
  needs WSL or Git Bash.
