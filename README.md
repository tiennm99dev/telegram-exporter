# telegram-exporter

Export a Telegram chat's media to a **WebDAV** server, using far less local disk
than the chat's total size.

`run.sh` runs [tdl](https://github.com/iyear/tdl) and
[rclone](https://rclone.org/) as a rolling pipeline: tdl downloads into a small
staging directory while rclone concurrently moves finished files to WebDAV and
deletes the local copies. Local disk only ever holds the files in flight plus
one sync interval of throughput, so a multi-terabyte chat exports fine on a
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

# 2) create the WebDAV remote
rclone config create tg-webdav webdav \
  url=https://dav.example.com/remote.php/dav/files/you \
  vendor=other user=YOU pass=SECRET

rclone lsd tg-webdav:      # verify it works
```

## Usage

```bash
./run.sh -r tg-webdav:tg-export -c @mygroup
```

That is the whole flow. It exports the chat's message metadata to
`export.json`, then downloads and uploads concurrently until finished. Progress
and warnings go to stderr; press Ctrl-C at any point and it stops cleanly.

### Options

| Flag | Meaning |
|------|---------|
| `-r REMOTE:PATH` | **Required.** rclone destination, e.g. `tg-webdav:tg-export` |
| `-c CHAT` | Chat to export when the JSON does not exist yet: `@username` or a numeric chat id |
| `-f FILE` | Export JSON to download from (default `export.json`) |
| `-d DIR` | Staging directory (default `./staging`) |
| `-i SECONDS` | Seconds between rclone sweeps (default `60`) |
| `-a AGE` | rclone `--min-age`, a second guard against moving files still being written (default `2m`) |
| `-h` | Help |

Anything after `--` is passed straight to `tdl dl`:

```bash
./run.sh -r tg-webdav:tg-export -- -t 4 -l 1      # calmer parallelism, fewer flood waits
./run.sh -r tg-webdav:tg-export -- -i mp4,mkv     # only these file extensions
./run.sh -r tg-webdav:tg-export -- -e jpg,png     # skip these file extensions
```

tdl defaults to `-t 8 -l 4`, which is aggressive; lower it if you hit flood
waits on a large export.

### Exporting a subset

Generate the JSON yourself when you want a narrower export, then point `-f` at
it:

```bash
tdl chat export -c @mygroup -T id -i 1000,5000 --all --with-content -o part.json
./run.sh -r tg-webdav:tg-export -f part.json
```

`tdl chat export` takes `-T time|id|last` with `-i` as the range, and `-f` as an
expression filter over message fields (`-f -` lists the available fields).

## Resuming

Re-run the same command. Both legs resume independently and nothing is
downloaded or uploaded twice.

Keep the same `export.json` between runs: `--skip-same` compares against the
**staging** directory, which is empty once files have moved to WebDAV, so
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

Exit codes: `0` success, `2` usage error, `3` rclone failure, `130`/`143`
interrupted, anything else is tdl's own exit code.

## Limits

- Streaming with no staging at all (piping download chunks straight into a
  WebDAV `PUT`) is not possible with tdl and would require custom code.
- The script is bash; the two tools it drives are cross-platform, but Windows
  needs WSL or Git Bash.
