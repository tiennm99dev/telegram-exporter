#!/usr/bin/env bash
#
# Drive run.sh repeatedly until every media file in a chat is on the remote.
#
# Each pass verifies what is already there, narrows the export JSON to just the
# ids still needed, and runs the pipeline on that subset. It stops when the
# verifier reports complete, when a pass makes no progress (the remaining media
# is genuinely unavailable on Telegram's side), or when the remote runs low on
# space.
#
# Usage: ./export-until-complete.sh -r REMOTE:PATH -c CHAT [options]
#   -r REMOTE:PATH  rclone destination (required)
#   -c CHAT         chat id/username, used for the initial metadata export
#   -f FILE         export JSON (default export-<chat>.json)
#   -d DIR          staging directory (default ./staging)
#   -i SECONDS      rclone sweep interval (default 120)
#   -p N            maximum passes (default 30)
#   -m SIZE         cap the staging directory at SIZE, e.g. 40G (default: no cap)
#   -q GIB          stop if remote free space falls below this (default 5)

set -euo pipefail

remote='' chat='' export_file='' staging='./staging' interval=120 max_staging=''
max_passes=30 min_free_gib=5

while getopts ':r:c:f:d:i:p:q:m:h' o; do case $o in
  r) remote=$OPTARG ;;   c) chat=$OPTARG ;;      f) export_file=$OPTARG ;;
  d) staging=$OPTARG ;;  i) interval=$OPTARG ;;  p) max_passes=$OPTARG ;;
  q) min_free_gib=$OPTARG ;; m) max_staging=$OPTARG ;;
  h) sed -n '2,19p' "$0"; exit 0 ;;
  *) echo "usage: $0 -r REMOTE:PATH -c CHAT [-f FILE] [-i SECS] [-p N] [-q GIB] [-m SIZE]" >&2; exit 2 ;;
esac; done

log() { printf '\n=== %s [driver] %s ===\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"; }

[[ -n $remote ]] || { echo "-r REMOTE:PATH is required" >&2; exit 2; }
[[ -n $chat || -n $export_file ]] || { echo "-c CHAT or -f FILE is required" >&2; exit 2; }
[[ -n $export_file ]] || export_file="export-${chat}.json"

# Stop the whole loop on Ctrl-C rather than rolling into the next pass. run.sh
# installs its own handlers, so a pass shuts down cleanly before we exit.
interrupted=0
trap 'interrupted=1' INT TERM

# tdl's progress bar is ANSI redraws: great on a terminal, unreadable in a log.
# Show it when stdout is a TTY, suppress it when output is redirected.
tdl_quiet=()
[[ -t 1 ]] || tdl_quiet=(--disable-progress-ps)

# The metadata export must exist before the first verify has anything to compare.
if [[ ! -f $export_file ]]; then
  [[ -n $chat ]] || { echo "$export_file missing and no -c CHAT to create it" >&2; exit 2; }
  log "exporting $chat metadata to $export_file"
  tdl chat export -c "$chat" --all --with-content -o "$export_file"
fi

free_gib() {
  rclone about "${remote%%:*}:" --json 2>/dev/null \
    | python3 -c 'import json,sys; print(int(json.load(sys.stdin).get("free",0))//2**30)' 2>/dev/null \
    || echo 999999   # backends without quota reporting must not block the run
}

prev_todo=-1
for ((pass = 1; pass <= max_passes; pass++)); do
  ((interrupted)) && { log 'interrupted; stopping'; exit 130; }

  log "pass $pass/$max_passes: verifying"
  if ./verify-export.sh -f "$export_file" -r "$remote" -d "$staging"; then
    log "complete after $((pass - 1)) download pass(es)"
    exit 0
  fi

  todo=$(wc -l < missing-ids.txt)
  if ((todo == prev_todo)); then
    log "no progress in the last pass; $todo file(s) look permanently unavailable"
    log 'ids left in missing-ids.txt'
    exit 1
  fi
  prev_todo=$todo

  free=$(free_gib)
  if ((free < min_free_gib)); then
    log "remote has only ${free} GiB free (limit ${min_free_gib}); stopping before it fills"
    exit 3
  fi
  log "$todo file(s) to fetch; ${free} GiB free on remote"

  # Narrow the full export to the outstanding ids, preserving its top-level shape
  # so tdl reads it exactly like the original.
  python3 - "$export_file" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
want = {int(l) for l in open('missing-ids.txt') if l.strip()}
d['messages'] = [m for m in d['messages'] if m['id'] in want]
json.dump(d, open('gap.json', 'w'))
print(f"gap.json: {len(d['messages'])} messages")
PY

  rc=0
  ./run.sh -r "$remote" -f gap.json -d "$staging" -i "$interval" \
    ${max_staging:+-m "$max_staging"} \
    -- --group=false -t 4 -l 2 ${tdl_quiet[@]+"${tdl_quiet[@]}"} || rc=$?
  log "pass $pass finished (run.sh exit $rc)"

  case $rc in
    0|1) ;;                                    # done or partial: verify decides
    3)   log 'rclone failure (remote full or unreachable); stopping'; exit 3 ;;
    130|143) log 'run interrupted; stopping'; exit 130 ;;
  esac
done

log "hit the $max_passes-pass limit; run again to continue"
exit 1
