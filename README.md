# telegram-exporter

Archive a Telegram chat's media to **any rclone remote** — S3, Google Drive,
Dropbox, Backblaze B2, SFTP, WebDAV, pikpak, or anything else rclone supports —
using far less local disk than the chat's total size.

`tgexport` embeds [tdl](https://github.com/iyear/tdl) and
[rclone](https://rclone.org/) as libraries and runs both halves in one process.
Files are downloaded into a small staging directory and uploaded the moment each
one finishes, so local disk only ever holds what is in flight. A multi-terabyte
chat archives fine on a small disk. Telegram caps a single file at 2 GB (4 GB
from premium uploaders), so a few dozen GB of staging covers the worst case
regardless of chat size.

This exists because **tdl can only write to a local directory** — it has no
remote destination of any kind (`tdl dl -d` takes a filesystem path; `tdl
--storage` is its session database, not an output target). tdl downloads over
MTProto with a user account, so Bot API limits do not apply: full history is
readable and there is no 20 MB download cap.

## Requirements

- **Go 1.25+** to build, or a prebuilt binary.
- **tdl** — only for `tdl login`. <https://docs.iyear.me/tdl/getting-started/installation/>
- **rclone** — only to configure a remote. <https://rclone.org/install/>

Neither tool is invoked at run time; `tgexport` reads the session and config
they write.

## Setup

Both steps are one-time.

```bash
tdl login                 # writes the Telegram session tgexport reads
rclone config             # define the destination remote
go build -o tgexport ./cmd/tgexport
./tgexport doctor -r myremote:archive
```

`doctor` proves both halves work before a long run: it prints the logged-in
account, resolves the destination, and reports free space.

### Build variants

| Build | Backends | Size |
|---|---|---|
| `go build ./cmd/tgexport` | every rclone backend | ~92 MB |
| `go build -tags slim ./cmd/tgexport` | pikpak only | ~49 MB |

A backend that is not compiled in does not exist at run time, so use the default
build unless the destination will never change.

## Usage

```bash
./tgexport sync -c CHAT -r REMOTE:PATH [options]
```

A run states what it found, what it is about to do, and then shows each file as
it moves:

```
reading mychannel
  12,000 messages read in 4m31s
indexing PikPak root 'mychannel'
  11,406 objects listed in 2m10s

  chat holds     12,000 media, 250.0 GiB
  archived       11,400
    never fetched 600
    wrong size    6

  fetching       606 files, 79.0 GiB (largest 2.0 GiB)
  into           PikPak root 'mychannel'
  staging        ./staging, capped at 40.0 GiB
  concurrency    2 download(s) x 4 thread(s), 2 upload(s)

  total     26/606 files [=>              ] 617.5 MiB / 79.0 GiB  2.5 MiB/s  8h47m
  ↓ …3214_4242_1000000000000000001.mp4   [=======>        ]  41.2 MiB / 96.0 MiB  1.8 MiB/s
  ↑ …3214_4243_1000000000000000002.mp4   ⠹                          uploading 1.9 GiB
```

Redirected output gets the same information as plain periodic lines plus one
line per archived file, with no cursor movement — a captured log stays readable.

`CHAT` accepts a numeric id as printed by `tdl chat ls`, a username with or
without `@`, or a `t.me`/`tg://` link. A Bot API `-100…` id is converted
automatically. A link to a single *message* is refused — it names a message, not
a chat.

```bash
# archive a chat, capping staging at 40 GiB
./tgexport sync -c @mychannel -r gdrive:telegram/media -m 40G

# check completeness without downloading anything
./tgexport verify -c @mychannel -r gdrive:telegram/media

# list what the chat holds
./tgexport list -c @mychannel
```

### Options

| Flag | Default | Meaning |
|---|---|---|
| `-c` | — | chat id, username, or link (required) |
| `-r` | — | rclone destination, `REMOTE:PATH` (required) |
| `-d` | `./staging` | staging directory for files in flight |
| `-m` | no cap | cap staging at a size, e.g. `40G` |
| `--threads` | 4 | connections per file |
| `--limit` | 2 | files downloading at once |
| `--uploads` | 2 | files uploading at once |
| `--min-free` | 5 | stop if the remote has fewer than this many GiB free, checked before and during the run |
| `--limit-items` | 0 | stop after N files; for smoke tests |
| `--confirm` | true | re-state each uploaded file to prove its size |
| `--takeout` | true | use a takeout session |
| `-n` | `default` | tdl session namespace |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | complete |
| 1 | ran, but files remain — run again |
| 2 | usage error |
| 3 | remote or Telegram failure, including a destination that stopped accepting uploads |
| 4 | stalled: files remain, none of which can ever be fetched |
| 130 / 143 | interrupted (SIGINT / SIGTERM) |

Only 1 is worth retrying. A driver looping until 0 should stop on anything else:
3 and 4 both mean the next pass would do exactly what this one did.

## How it works

Re-running is the resume path. Each item is checked against a listing of the
remote immediately before download, so an interrupted run picks up where it left
off and a completed one downloads nothing.

**Filenames.** Every file is stored as `{DialogID}_{MessageID}_{FileName}`, where
`FileName` is exactly what Telegram reports. One function derives that string,
and the same string is used both to ask whether the file is already archived and
to write it — so the two can never disagree.

A name that cannot survive that round trip is refused rather than rewritten: too
long for the filesystem once `.part` is appended, not a single path element, or
containing a character rclone's path encoder rewrites (control bytes, `DEL`, and
the encoder's own escape character). Those files are reported under
`unarchivable` and never counted as present. Rewriting them is what the next
paragraph is about.

That last point is the reason this program exists. Its predecessor derived the
name twice: `tdl chat export` wrote the raw name into a JSON, while `tdl dl`
rendered it through a template applying `filenamify`, which rewrites characters a
filesystem rejects and collapses runs of `!`. A file whose name contained `!!`
was looked up under one name and stored under another, so the verifier never
found it and re-fetched it on every pass — forever, at 966 MB a time.

Note the consequence: names are **not** run through `filenamify`, so they are not
byte-compatible with what the old shell pipeline wrote. A file it stored under a
rewritten name will not be recognised and gets fetched again.

**Disk.** `-m` is a byte budget. A download reserves its own size before starting
and releases it only once the upload is confirmed, so when the remote is slow the
downloads pause on their own. The cap must exceed the largest single file, and a
cap that does not is refused at startup rather than discovered as a hang.

**Integrity.** A download is written to `<name>.part` and renamed only once its
size matches what Telegram reported, so a file without the suffix is always
whole. Uploads are re-stated afterwards to prove they arrived at the right size,
before the local copy is gone, and an object that turns out short is deleted
rather than left under a name a later run would trust.

`verify` compares stored sizes against what Telegram reports, so a truncated
object is outstanding rather than "present". This is stricter than the shell
verifier, which matched on name and non-zero size — on the archive this was
built for it found six objects that had been counted complete for months, one
of them 221 MiB standing in for a 2 GiB video. Re-running repairs them.

## Replacing the shell pipeline

Earlier versions of this repo were three bash scripts — `run.sh`,
`export-until-complete.sh` and `verify-export.sh` — driving `tdl` and `rclone` as
separate processes. Everything expensive in them existed to work around the fact
that neither process could see the other's state: a staging directory polled with
`du -sk`, an `--min-age` guard, a `*.tmp` exclusion, `SIGSTOP`/`SIGCONT` to
enforce the disk cap, a sweep-failure counter, and an outer loop that re-verified
and re-narrowed a JSON export between passes.

One process needs none of it. Completion is a function returning; the cap is a
semaphore. Some hard-won details were worth keeping, and are:

- **pikpak commits uploads as a server-side async task**, and rclone abandons a
  still-pending one when its low-level retries run out. `transfers=2` and
  `low-level-retries=20` are the defaults here for that reason. Environment
  overrides still win.
- **A backend with no quota API is treated as unlimited**, so it never blocks a
  run.
- **Zero-byte files count as missing** — rclone overwrites a size-mismatched
  destination, so re-running repairs them — while files under 1 KiB are reported
  but trusted, since some real media genuinely is that small.
- **Indexing ignores `RCLONE_*` filters.** The transfer tunables above are
  deliberately env-overridable; the listing is not. A stray `RCLONE_EXCLUDE` or
  `RCLONE_MIN_SIZE` left over from another job would otherwise narrow the index
  and re-download everything it hid.

## Notes

- `tgexport` and the `tdl` CLI share one session store and cannot run against the
  same namespace at once. Use `-n` for a second namespace if you need both.
- A partially downloaded file is not resumable across restarts — tdl's library
  exposes no resume offset — so an interrupted run re-fetches whatever was in
  flight, bounded by `--limit`.
- Everything is read-only against Telegram. Nothing is uploaded, deleted, or
  marked read.
