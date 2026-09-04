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
max_staging=''      # empty: staging grows as fast as tdl fills it
max_sync_failures=5
cap_check_interval=10   # seconds between -m checks, independent of -i

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
  -m SIZE         cap the staging directory at SIZE (K/M/G/T, binary; e.g.
                  40G). When staging reaches it, tdl is suspended until
                  rclone has drained the finished files. Unset means no cap.
  -h              this help

Everything after -- is appended to the 'tdl dl' command, e.g.
  $PROG -r gdrive:telegram/media -- -t 4 -l 1
USAGE
}

log() { printf '%s [%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$PROG" "$*" >&2; }
die() { log "error: $*"; exit 2; }   # usage or precondition
fail() { log "error: $*"; exit 3; }  # rclone / pipeline failure

while getopts ':f:c:d:r:i:a:m:h' opt; do
  case $opt in
    f) export_file=$OPTARG ;;
    c) chat=$OPTARG ;;
    d) staging=$OPTARG ;;
    r) remote=$OPTARG ;;
    i) interval=$OPTARG ;;
    a) min_age=$OPTARG ;;
    m) max_staging=$OPTARG ;;
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

# Sizes are compared in KiB because that is the unit 'du -sk' reports, which is
# also the unit that matters here: allocated blocks, not apparent length.
max_staging_kib=''
if [[ -n $max_staging ]]; then
  [[ $max_staging =~ ^[0-9]+[KkMmGgTt]?$ ]] \
    || die "-m must be a size with an optional K/M/G/T suffix, e.g. 40G"
  num=${max_staging%[KkMmGgTt]}
  case ${max_staging#"$num"} in
    ''|K|k) max_staging_kib=$num ;;
    M|m) max_staging_kib=$((num * 1024)) ;;
    G|g) max_staging_kib=$((num * 1024 * 1024)) ;;
    T|t) max_staging_kib=$((num * 1024 * 1024 * 1024)) ;;
  esac
  ((max_staging_kib > 0)) || die "-m must be greater than zero"
fi

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

# The sweeps that run once at the end have a whole staging directory to move
# and no interleaved tdl output, so they report progress instead of going quiet
# for several minutes. rclone's redrawn bar is right on a terminal but turns a
# redirected log into control characters, so a log gets periodic one-line
# stats. Those are logged at INFO, which '-v' would enable at the cost of a
# line per file, hence raising the stats to NOTICE rather than the whole log.
sweep_progress=(--progress)
[[ -t 1 ]] || sweep_progress=(--stats 30s --stats-one-line --stats-log-level NOTICE)

# Move whatever is finished. Partial .tmp files are always excluded. $1 is an
# optional --min-age guard; $2 enables --delete-empty-src-dirs, which is safe
# only once tdl has stopped — rclone removing a directory between tdl's
# MkdirAll and Create makes tdl fail, and it can take the staging root too,
# hence the mkdir afterwards. $3 turns on the progress reporting above.
sweep() {
  local rc=0
  local args=(--exclude "$TEMP_GLOB")
  if [[ -n ${1:-} ]]; then args+=(--min-age "$1"); fi
  if ((${2:-0})); then args+=(--delete-empty-src-dirs); fi
  if ((${3:-0})); then args+=(${sweep_progress[@]+"${sweep_progress[@]}"}); fi
  rclone move "$staging" "$remote" "${args[@]}" || rc=$?
  mkdir -p "$staging"
  return $rc
}

staging_kib() { du -sk "$staging" 2>/dev/null | cut -f1; }

over_cap() {
  local used
  used=$(staging_kib)
  [[ -n $used ]] && ((used >= max_staging_kib))
}

# Enforce -m. tdl renames a file only once it is complete, so staging holds
# finished files plus the in-flight '*.tmp' ones, and only the former can be
# drained. Suspending tdl stops it adding more while rclone empties the
# directory; SIGSTOP is safe because tdl reconnects on resume and --continue
# picks its .tmp files back up. The cap must therefore stay well above what the
# concurrent downloads hold, or draining could never clear it.
drain_to_cap() {
  local used rc=0
  used=$(staging_kib)
  log "staging at $((used / 1024)) MiB, at or over the $((max_staging_kib / 1024)) MiB cap; suspending tdl to drain"

  kill -STOP "$tdl_pid" 2>/dev/null || true
  # Sweep until staging is back under the cap, since one rclone move need not
  # get there: a slow remote or a per-file error can leave finished files
  # behind. Stop as soon as a sweep frees nothing, which means all that is
  # left is in-flight .tmp files that no sweep can ever move.
  local before
  while :; do
    before=$used
    # No --min-age: tdl is frozen, so every non-.tmp file is finished by
    # construction and waiting out the guard would only prolong the pause.
    sweep '' || { rc=$?; break; }
    used=$(staging_kib)
    [[ -n $used ]] || break
    ((used >= max_staging_kib && used < before)) || break
  done
  kill -CONT "$tdl_pid" 2>/dev/null || true

  if ((rc == 0)) && [[ -n $used ]] && ((used >= max_staging_kib)); then
    log "warning: staging is still $((used / 1024)) MiB after draining — the cap is below what the in-flight downloads hold; raise -m or lower tdl's -l/-t"
  else
    log "resumed tdl, staging now $((${used:-0} / 1024)) MiB"
  fi
  return $rc
}

cleanup() {
  if [[ -n $tdl_pid ]] && kill -0 "$tdl_pid" 2>/dev/null; then
    log "stopping tdl (pid $tdl_pid)"
    # A suspended process never sees SIGTERM, so let it run first.
    kill -CONT "$tdl_pid" 2>/dev/null || true
    kill -TERM "$tdl_pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 "$tdl_pid" 2>/dev/null || break
      sleep 1
    done
    kill -KILL "$tdl_pid" 2>/dev/null || true
  fi
  if ((sweep_ok)); then
    log 'sweeping completed files before exit'
    sweep "$min_age" 0 1 || log 'warning: final safety sweep failed; staging kept'
  fi
}
trap cleanup EXIT
trap 'log "interrupted (SIGINT)"; exit 130' INT
trap 'log "terminated (SIGTERM)"; exit 143' TERM

log "downloading into $staging, moving to $remote every ${interval}s${max_staging_kib:+, capped at $((max_staging_kib / 1024)) MiB}"
tdl dl -f "$export_file" -d "$staging" \
  --takeout --group --skip-same --continue ${tdl_extra[@]+"${tdl_extra[@]}"} &
tdl_pid=$!
sweep_ok=1

failures=0
waited=0
while kill -0 "$tdl_pid" 2>/dev/null; do
  # A long sweep interval must not let staging blow past the cap in between, so
  # sleep in slices and check the cap on each one. Every sleep is a job, so
  # signals are handled without waiting the slice out.
  slice=$((interval - waited))
  if [[ -n $max_staging_kib ]] && ((slice > cap_check_interval)); then
    slice=$cap_check_interval
  fi
  sleep "$slice" &
  wait $! 2>/dev/null || true
  waited=$((waited + slice))

  # Only a sweep that actually ran says anything about rclone's health, so the
  # failure streak is judged on those alone and a quiet cap check never
  # clears it.
  rc=0
  swept=0
  if ((waited >= interval)); then
    waited=0
    swept=1
    sweep "$min_age" || rc=$?
  fi
  if ((rc == 0)) && [[ -n $max_staging_kib ]] && kill -0 "$tdl_pid" 2>/dev/null; then
    if over_cap; then
      swept=1
      drain_to_cap || rc=$?
    fi
  fi

  if ((swept)); then
    if ((rc == 0)); then
      failures=0
    else
      failures=$((failures + 1))
      log "warning: rclone sweep failed ($failures/$max_sync_failures)"
      if ((failures >= max_sync_failures)); then
        sweep_ok=0
        fail "rclone failed $failures times in a row; stopping before staging fills the disk"
      fi
    fi
  fi
done

wait "$tdl_pid" || tdl_rc=$?
tdl_pid=''
sweep_ok=0

if ((tdl_rc != 0)); then
  log "tdl exited $tdl_rc; staging kept at $staging — re-run to resume"
  sweep "$min_age" 0 1 || log 'warning: sweep after failure did not complete'
  exit "$tdl_rc"
fi

log 'tdl finished; final sweep'
sweep '' 1 1 || fail "final sweep failed; files remain in $staging"
log "done — everything moved to $remote"
