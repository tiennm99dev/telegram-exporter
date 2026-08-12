# telegram-exporter

`tg-export` downloads **all media from a Telegram group** to local disk, one
folder per logical post (albums collapsed), with a `messages.jsonl` sidecar that
links every file back to its message. Resumable, filterable, with a dry-run size
estimate.

```bash
export TG_API_ID=... TG_API_HASH=...

tg-export --group @mygroup --out ./exports --dry-run   # what would it fetch?
tg-export --group @mygroup --out ./exports             # fetch it
```

Kill it at any point and re-run the same command; it resumes without
re-downloading and without gaps.

## Why the Bot API cannot do this

This is the first question most people ask, so: a bot cannot read a group's
message history at all, and the Bot API caps file downloads at 20 MB. Both limits
are structural, not configuration. Exporting history therefore requires MTProto
with a **user account**, which is what Telethon provides.

## Install

Python 3.12+.

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt -e .
.venv/bin/tg-export --help
```

`requirements.txt` pins the exact Telethon version the API probes were verified
against. Telethon 2.0 is alpha with a renamed API surface, hence the `<2` bound.

`cryptg` is **optional and not installed**. It is a native extension sitting in
Telethon's AES path, so it sees the auth key and every plaintext byte. It ships an
aarch64 wheel (no build step), and the reason to trust it *if* you adopt it is
that the Telethon author maintains it — not that it is "CPU-side only". Do not add
a supply-chain root for a throughput problem you have not measured.

## Credentials

Create an app at <https://my.telegram.org> → API development tools.

| Credential | Source | Why |
|---|---|---|
| `api_id`, `api_hash` | env `TG_API_ID` / `TG_API_HASH` only | needed every run; revocable |
| phone | prompt; `TG_PHONE` optional | PII, not a secret |
| login code | **prompt only** | single-use, 5-minute TTL |
| 2FA password | **`getpass` only** — no env, no file, no flag | once per session lifetime |

`.env` **may** contain `TG_API_ID`, `TG_API_HASH`, `TG_PHONE`. It **must not**
contain the 2FA password, a login code, or session data. The app does not read
`.env` — your shell already does that:

```bash
set -a; . ./.env; set +a
```

### First run and the session file

The first run prompts for your phone, the login code Telegram sends you, and your
2FA password if you have one. Later runs are non-interactive.

The session file defaults to
`${XDG_STATE_HOME:-~/.local/state}/tg-export/default.session` — **outside the
repo**, so the safe location needs no opt-in. It and every SQLite sibling are mode
`0600`, enforced by a `umask(0o077)` set before anything can create a file, plus a
post-login assertion. A `chmod` after connecting would be too late: SQLite writes
the auth key *during* login.

If you point `--session` or `--out` inside a git repo at a path that is not
gitignored, the run refuses with an explanation. Deleting a session file does
**not** revoke it server-side — use Telegram → Settings → Devices for that.

## Output layout

```
exports/g-1001234567890/     <- directory name is the chat id, never the group title
  title.txt                  <- the human-readable name lives here, as data
  1042/                      <- one album (shared grouped_id); folder = first member id
    1042_photo.jpg           <- filename keyed on message id, not a positional index
    1043_photo.jpg
    1044_video.mp4
  1047/                      <- a standalone message
    1047_document.pdf
  messages.jsonl
  .export-state.json
  .export.lock
```

The directory is `g<chat_id>` and not the group title because the title is
server-supplied and editable by any admin. A title of `..` or `a/b` would be a
path-traversal vector that no filename sanitizer could catch, since the escape
happens in a parent component. Naming the directory after an int makes that
structurally impossible — and also means a mid-export group rename cannot orphan
your download. The cost is that `ls exports/` shows numbers.

Filenames are keyed on the **message id** rather than a position in the album,
because a positional index shifts when a member message is deleted between runs,
silently orphaning files already on disk.

## Resume semantics

`.export-state.json` holds one cursor, and it means:

> every post with `max_message_id <= cursor_id` has been handled **under these
> filters**.

Consequences worth knowing:

- The cursor is written only after a post is completely handled, and never from an
  error path. It can never point past in-flight work.
- Filters are stored verbatim. Changing them refuses the run and prints exactly
  what changed, because the cursor's meaning depends on them:

  ```
  filter mismatch:
    types:       ['photo', 'video'] -> ['video']
    max_size:    100000000 -> (none)
  re-run with the original filters, or --reset-state to start over
  ```

- `--reset-state` discards the cursor and rotates `messages.jsonl` to
  `messages-<utc-timestamp>.jsonl`. It does **not** delete downloaded files —
  dedupe reclaims them on the next run, and deleting 30 GB to change a filter
  would be hostile.
- `--limit N` is *not* part of the filter set. It changes when we stop, not what a
  post contains, so a `--limit 20` test run is a genuine resumable partial export
  that does not poison later full runs.
- `completed_at` is set only when a run exhausts the history cleanly. It is the
  only way to answer "did my 20-hour export actually finish?".
- **A file that fails is recorded and skipped, and the cursor moves past it.** The
  run continues and the exit summary names the count (`N files failed (see
  messages.jsonl)`). Media that was deleted or expired is genuinely gone, but a
  file that exhausted its retries is not re-attempted by a later resume — grep
  `errors` in `messages.jsonl` to find them, and use `--reset-state` to re-drive
  that range if it matters.

## `messages.jsonl`

```json
{"message_id":1043,"post_id":1042,"grouped_id":88,"date":"2026-07-04T12:00:00+00:00",
 "sender_id":777,"sender_name":"Alice","caption":"beach trip","reply_to":1001,
 "files":[{"path":"1042/1043_photo.jpg","size":1234567,"name":"photo.jpg",
           "mime":"image/jpeg","kind":"photo","declared_size":1234999}],
 "errors":[]}
```

**Each record is an append-only event: "the files written for this message in this
run."** So the correct way to read the sidecar is the **union of all records for a
`message_id`**, never last-write-wins — after a filter change, the most recent
record deliberately describes a narrower slice than what is on disk.

- `size` is bytes on disk; `declared_size` appears only when the server's number
  differs (see the photo note below).
- Paths are relative to the export root.
- `sender_name` is filled in only when the sender was already cached. Resolving it
  otherwise would mean one extra RPC per message — 100k RPCs and a flood ban on a
  large group, in exchange for a display string.
- A partial final line from a host crash is truncated and warned about at startup.
- With `--include-text`, text-only messages get the same record shape with
  `"files": []`. Distinguish media from context by `files` being empty, not by a
  record type.

## Flags

```
tg-export --group <id|@username|link> --out DIR
          [--dry-run] [--since YYYY-MM-DD] [--until YYYY-MM-DD]
          [--types photo,video,document,audio,voice] [--max-size 100MB]
          [--include-text] [--limit N] [--session PATH] [--reset-state]
          [--min-free 2GiB] [--max-flood-wait SECONDS] [--verbose]
```

- `--include-text` is off by default. When set, text-only messages are recorded in
  the sidecar with an empty `files[]` so captions and replies keep their
  surrounding conversation. They never create a folder and never download
  anything. It **is** part of the stored filter set — it changes what a post
  contains — so toggling it requires `--reset-state`.
- `--verbose` raises the log level for *this tool only*. The `telethon` logger is
  pinned at `WARNING` unconditionally, because its request logging carries
  `api_id`, `phone_number` and `phone_code_hash`. There is deliberately no
  `--debug-telethon`.
- A link preview is not media: a message containing a URL is not treated as an
  attachment, so preview thumbnails are never downloaded.

## Flood waits have no ceiling by default

Telegram throttles by telling the client to wait, and those waits escalate from
seconds to hours if you keep pushing. By default `tg-export` **sleeps as long as
Telegram demands**, so an unattended export completes rather than failing fast.

Every wait is logged at `WARNING` with its duration **and its computed wake
time**:

```
2026-08-11 07:42:11 WARNING flood wait 3600s - sleeping until 2026-08-11T08:42:11+00:00
```

A long sleep is a sleep, not a hang. Read the log before assuming otherwise. Pass
`--max-flood-wait SECONDS` to exit cleanly and resumably (code 6) instead.

## Why downloads are sequential

Telethon does not parallelize a single transfer, and running several at once
mainly accelerates flood-wait escalation — a 7-second wait becomes a 4-hour
lockout. There is deliberately no `--workers` flag, and an AST test in the suite
fails if `gather`, `create_task`, `to_thread` or a thread pool ever appears in
`src/`.

## Disk space

`--dry-run` reports remaining posts, files and bytes by media type against free
space, and exits 2 if it will not fit.

**That verdict is advisory.** The enforcement is a per-file check before each
download, comparing free space against *this file's* size plus the `--min-free`
reserve. So a `SHORT` verdict does not mean the run is unsafe — it means the run
will stop partway with a clean, resumable cursor.

Photo totals are labelled approximate because a photo's declared `.size`
describes the size variant Telethon selected. Documents, video and audio carry an
exact size. Media reporting no size at all is downloaded anyway and counted on its
own `unknown size` line rather than as zero, so the estimate never hides an
unbounded error.

`--dry-run` reads the cursor and measures from where a real run would start, so
`tg-export --dry-run && tg-export` works on a partially completed export. It takes
no lock, creates no directories, and mutates nothing — you can run it while a real
export is in progress.

## Concurrent runs

One run per export root. A second run against the same `--out` exits 3 and names
the holder's pid, host and start time. `flock` releases on process death, `kill -9`
included, so there is no stale lock to clean up.

Exporting two *different* groups at the same time works, but each needs its own
`--session` as well as its own `--out`: the session file is a SQLite database and
is locked too, because two Telethon clients sharing one session is a corruption
hazard Telethon itself warns about.

`--dry-run` takes no lock at all, so it can always be run against an export that
is currently in progress.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unexpected error, or a bad argument |
| 2 | `--dry-run` says it will not fit |
| 3 | disk exhausted, or another run holds the lock |
| 4 | session invalidated — re-login |
| 5 | lost access to the group |
| 6 | flood wait exceeded `--max-flood-wait` (only when you set it) |
| 7 | filter or chat mismatch against the existing state file |
| 130 | interrupted — re-run to resume |

## Development

```bash
.venv/bin/pip install -r requirements.txt -e ".[dev]"
.venv/bin/pytest
```

The whole suite runs offline against synthetic message stubs — no credentials, no
network. Note that `messages.jsonl` contains message text and sender names from
other people; the export tree is gitignored and the runtime check refuses an
un-ignored path inside a repo, but check `git status` before any commit.
