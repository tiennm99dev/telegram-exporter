#!/usr/bin/env bash
#
# Rolling pipeline: tdl downloads Telegram media into a small staging directory
# while rclone concurrently moves finished files to any rclone remote (S3,
# Google Drive, SFTP, WebDAV, B2, ...). Local disk only ever holds the files in
# flight plus one sync interval of throughput, so a chat larger than the local
# disk can still be exported.
#
# See README.md for the background.
#
# Exit codes: 0 ok, 2 usage error, 3 rclone failure, 130/143 interrupted,
# anything else is tdl's own exit code.

set -euo pipefail

readonly PROG=${0##*/}

# tdl downloads to '<name>.tmp' and renames only on completion, so an unfinished
# file is always identifiable by extension. Never move one: a download stalled
# by a flood wait stops touching its .tmp, which then ages past --min-age and
# would be uploaded half-written, destroying tdl's resume point for that file.
readonly TEMP_GLOB='*.tmp'

# Defaults
export_file=''      # decided after parsing: per-chat when -c is given
chat=''
staging='./staging'
remote=''
interval=60
min_age='2m'
max_sync_failures=5

usage() {
  cat <<USAGE
Usage: $PROG -r REMOTE:PATH [options] [-- extra tdl dl args...]

Required:
  -r REMOTE:PATH  rclone destination, e.g. gdrive:telegram/media

Options:
  -f FILE         tdl export JSON (default: export-<chat>.json with -c,
                  otherwise export.json)
  -c CHAT         chat to export when FILE does not exist. Accepts a numeric
                  id as printed by 'tdl chat ls', a username with or without
                  '@', or a t.me/tg:// link. A Bot API '-100...' id is
                  converted to the plain id tdl expects.
  -d DIR          staging directory (default: $staging)
  -i SECONDS      seconds between rclone sweeps (default: $interval)
  -a AGE          rclone --min-age, a second guard against moving files still
                  being written (default: $min_age)
  -h              this help

Everything after -- is appended to the 'tdl dl' command, e.g.
  $PROG -r gdrive:telegram/media -- -t 4 -l 1
USAGE
}

log() { printf '%s [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$PROG" "$*" >&2; }
die() { log "error: $*"; exit 2; }   # usage or precondition
fail() { log "error: $*"; exit 3; }  # rclone / pipeline failure

while getopts ':f:c:d:r:i:a:h' opt; do
  case $opt in
    f) export_file=$OPTARG ;;
    c) chat=$OPTARG ;;
    d) staging=$OPTARG ;;
    r) remote=$OPTARG ;;
    i) interval=$OPTARG ;;
    a) min_age=$OPTARG ;;
    h) usage; exit 0 ;;
    :) die "option -$OPTARG requires an argument" ;;
    ?) die "unknown option -$OPTARG (try -h)" ;;
  esac
done
shift $((OPTIND - 1))
tdl_extra=("$@")

[[ -n $remote ]] || { usage >&2; die "-r REMOTE:PATH is required"; }
[[ $remote == *:* ]] || die "remote '$remote' is not in rclone REMOTE:PATH form"
[[ $interval =~ ^[0-9]+$ && $interval -gt 0 ]] || die "-i must be a positive integer"
[[ $min_age =~ ^[0-9]+(\.[0-9]+)?(ms|s|m|h|d|w|M|y)?$ ]] \
  || die "-a must be an rclone duration, e.g. 2m"

# tdl resolves a numeric argument as an MTProto id and anything else through
# gotd's resolver, which handles '@name', 'name' and t.me/tg:// links. Two forms
# still need help: Bot API ids carry a '-100' prefix that MTProto does not use,
# and a message link is not a chat.
if [[ -n $chat ]]; then
  read -r chat <<<"$chat"        # trim stray whitespace
  case $chat in
    '') die "-c requires a chat" ;;
    *t.me/c/*|*t.me/*/[0-9]*)
      die "-c takes a chat, not a message link ($chat) — pass the chat's username or id" ;;
    -100[0-9]*)
      log "converting Bot API id $chat to MTProto id ${chat#-100}"
      chat=${chat#-100} ;;
  esac
fi

# Keep each chat's export in its own file, so switching -c never silently
# downloads the previous chat again from a stale export.json.
if [[ -z $export_file ]]; then
  if [[ -n $chat ]]; then
    slug=${chat#@}
    slug=${slug##*/}
    slug=$(printf '%s' "$slug" | tr -c 'A-Za-z0-9._-' '_')
    export_file="export-$slug.json"
  else
    export_file='export.json'
  fi
fi

for tool in tdl rclone; do
  command -v "$tool" >/dev/null || die "$tool is not installed or not on PATH"
done

# A named remote must exist in the config; a leading ':' means an on-the-fly
# connection string, which has no config entry to check. Listing the remote's
# root is not portable (some backends refuse it), so reachability and
# credentials are proven by creating the destination, which rclone would create
# on the first move anyway.
if [[ $remote != :* ]]; then
  # Read the list into a variable first: piping it into 'grep -q' lets grep exit
  # on the first match and kill rclone with SIGPIPE, which pipefail then reports
  # as a failed pipeline -- rejecting a remote that is in fact configured.
  remotes=$(rclone listremotes 2>/dev/null || true)
  grep -qx -- "${remote%%:*}:" <<<"$remotes" \
    || die "rclone remote '${remote%%:*}:' is not configured — see 'rclone listremotes'"
fi
rclone mkdir "$remote" >/dev/null 2>&1 \
  || die "cannot reach '$remote' — check credentials and connectivity"

# The export JSON only lists messages; it is cheap to keep and required for both
# legs to stay resumable, so never regenerate it when it already exists.
if [[ ! -f $export_file ]]; then
  [[ -n $chat ]] || die "$export_file not found; pass -c CHAT to export it first"
  log "exporting $chat metadata to $export_file"
  tdl chat export -c "$chat" --all --with-content -o "$export_file"
else
  log "using existing $export_file (delete it to re-export)"
fi

mkdir -p "$staging"

tdl_pid=''
tdl_rc=0
sweep_ok=0

# Move whatever is finished. Partial .tmp files are always excluded. $1 is an
# optional --min-age guard; $2 enables --delete-empty-src-dirs, which is safe
# only once tdl has stopped — rclone removing a directory between tdl's
# MkdirAll and Create makes tdl fail, and it can take the staging root too,
# hence the mkdir afterwards.
sweep() {
  local rc=0
  local args=(--exclude "$TEMP_GLOB")
  if [[ -n ${1:-} ]]; then args+=(--min-age "$1"); fi
  if ((${2:-0})); then args+=(--delete-empty-src-dirs); fi
  rclone move "$staging" "$remote" "${args[@]}" || rc=$?
  mkdir -p "$staging"
  return $rc
}

cleanup() {
  if [[ -n $tdl_pid ]] && kill -0 "$tdl_pid" 2>/dev/null; then
    log "stopping tdl (pid $tdl_pid)"
    kill -TERM "$tdl_pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 "$tdl_pid" 2>/dev/null || break
      sleep 1
    done
    kill -KILL "$tdl_pid" 2>/dev/null || true
  fi
  if ((sweep_ok)); then
    log 'sweeping completed files before exit'
    sweep "$min_age" || log 'warning: final safety sweep failed; staging kept'
  fi
}
trap cleanup EXIT
trap 'log "interrupted (SIGINT)"; exit 130' INT
trap 'log "terminated (SIGTERM)"; exit 143' TERM

log "downloading into $staging, moving to $remote every ${interval}s"
tdl dl -f "$export_file" -d "$staging" \
  --takeout --group --skip-same --continue ${tdl_extra[@]+"${tdl_extra[@]}"} &
tdl_pid=$!
sweep_ok=1

failures=0
while kill -0 "$tdl_pid" 2>/dev/null; do
  # sleep as a job so signals are handled without waiting out the interval
  sleep "$interval" &
  wait $! 2>/dev/null || true

  if sweep "$min_age"; then
    failures=0
  else
    failures=$((failures + 1))
    log "warning: rclone sweep failed ($failures/$max_sync_failures)"
    if ((failures >= max_sync_failures)); then
      sweep_ok=0
      fail "rclone failed $failures times in a row; stopping before staging fills the disk"
    fi
  fi
done

wait "$tdl_pid" || tdl_rc=$?
tdl_pid=''
sweep_ok=0

if ((tdl_rc != 0)); then
  log "tdl exited $tdl_rc; staging kept at $staging — re-run to resume"
  sweep "$min_age" || log 'warning: sweep after failure did not complete'
  exit "$tdl_rc"
fi

log 'tdl finished; final sweep'
sweep '' 1 || fail "final sweep failed; files remain in $staging"
log "done — everything moved to $remote"
