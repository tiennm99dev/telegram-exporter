#!/usr/bin/env bash
#
# Verify a tdl/rclone export is complete and every file is intact.
#
# Rebuilds the filename tdl produces for each media message in the export JSON
# ({DialogID}_{MessageID}_{FileName}) and checks it exists on the remote or in
# staging. Zero-byte files count as missing: rclone overwrites a size-mismatched
# destination, so re-running repairs them. Files under 1 KiB are reported for
# inspection but trusted, since some real media is genuinely that small.
#
# Writes every id needing another attempt to missing-ids.txt.
# Exit 0 = complete, 1 = incomplete, 2 = usage error.
#
# Usage: ./verify-export.sh -f export-<chat>.json -r REMOTE:PATH [-d STAGING]

set -euo pipefail

export_file='' remote='' staging='./staging'
while getopts ':f:r:d:h' o; do case $o in
  f) export_file=$OPTARG ;; r) remote=$OPTARG ;; d) staging=$OPTARG ;;
  h) sed -n '2,16p' "$0"; exit 0 ;;
  *) echo "usage: $0 -f FILE -r REMOTE:PATH [-d STAGING]" >&2; exit 2 ;;
esac; done

[[ -f $export_file ]] || { echo "no such export file: $export_file" >&2; exit 2; }
[[ -n $remote ]] || { echo "-r REMOTE:PATH is required" >&2; exit 2; }

listing=$(mktemp); trap 'rm -f "$listing"' EXIT
# lsl gives sizes as well as names, so truncated uploads are detectable.
rclone lsl "$remote" > "$listing"

python3 - "$export_file" "$listing" "$staging" <<'PY'
import json, os, re, sys
export_file, listing, staging = sys.argv[1], sys.argv[2], sys.argv[3]

sizes = {}
for line in open(listing):
    m = re.match(r'^\s*(\d+)\s+\S+\s+\S+\s+(.*)$', line.rstrip('\n'))
    if m:
        sizes[m.group(2)] = int(m.group(1))
if os.path.isdir(staging):
    for f in os.listdir(staging):
        if not f.endswith('.tmp'):
            sizes.setdefault(f, os.path.getsize(os.path.join(staging, f)))

msgs = json.load(open(export_file))['messages']
# Text-only messages carry an empty "file" and are not download targets.
media = [m for m in msgs if m.get('file')]

dialog = str(json.load(open(export_file)).get('id', '')).lstrip('-')
if not dialog.isdigit():
    ids = {n.split('_')[0] for n in sizes if '_' in n}
    dialog = ids.pop() if len(ids) == 1 else ''
if not dialog:
    sys.exit('cannot determine dialog id')

absent, empty, tiny = [], [], []
for m in media:
    name = f"{dialog}_{m['id']}_{m['file']}"
    if name not in sizes:
        absent.append(m['id'])
    elif sizes[name] == 0:
        empty.append(m['id'])
    elif sizes[name] < 1024:
        tiny.append((m['id'], sizes[name], name))

todo = sorted(absent + empty)
print(f"messages in export : {len(msgs)}")
print(f"  text-only (skip) : {len(msgs) - len(media)}")
print(f"  media expected   : {len(media)}")
print(f"present and intact : {len(media) - len(todo)}")
print(f"  absent           : {len(absent)}")
print(f"  zero-byte        : {len(empty)}")
if tiny:
    print(f"  under 1KiB (check, not retried): {len(tiny)}")
    for i, s, n in tiny[:5]:
        print(f"      id {i}  {s} B  {n}")

if todo:
    with open('missing-ids.txt', 'w') as fh:
        fh.write('\n'.join(map(str, todo)) + '\n')
    print(f"\nneeds another pass : {len(todo)}  (ids {min(todo)}–{max(todo)})")
    print("written to missing-ids.txt")
    sys.exit(1)

if os.path.exists('missing-ids.txt'):
    os.remove('missing-ids.txt')
print("\nCOMPLETE: every media message is present and non-empty.")
PY
