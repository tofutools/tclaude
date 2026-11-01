#!/usr/bin/env bash
# Files docs-drift findings as awb tickets, once each. A finding's fingerprint
# comes from its doc page and the words it quotes, not its line number, and goes
# in the ticket. A finding whose fingerprint is on any ticket, closed ones included,
# is skipped, so closing a ticket as "not drift" also silences it.
#
#   scripts/docs/drift-file.sh findings.json                 show what would be filed
#   scripts/docs/drift-file.sh --file -w tcl findings.json   file it
#
# findings.json is an array of objects with these fields: title, doc, line, quote,
# ui_shows, capture (a tour image) and fix (the proposed wording).
set -euo pipefail

file=0 workspace=()
while (( $# > 1 )); do
  case "$1" in
    --file) file=1 ;;
    -w) workspace=(-w "$2"); shift ;;
    *) break ;;
  esac
  shift
done
if (( $# != 1 )); then
  echo "usage: $0 [--file] [-w WORKSPACE] findings.json" >&2
  exit 2
fi
findings="$1"

missing="$(jq -r '.[] | select([.title, .doc, .line, .quote, .ui_shows, .capture, .fix] | any(. == null or . == "")) | .title // .doc // "untitled"' "$findings")"
if [[ -n "$missing" ]]; then
  echo "findings with missing evidence, nothing filed:" >&2
  echo "$missing" >&2
  exit 1
fi
base="$(dirname "$findings")"
for capture in $(jq -r '.[].capture' "$findings"); do
  [[ -f "$capture" || -f "$base/$capture" ]] || { echo "no such capture: $capture" >&2; exit 1; }
done

fingerprint() { # fingerprint <doc> <quote>
  local quote
  quote="$(tr '[:upper:]' '[:lower:]' <<<"$2" | tr -s '[:space:]' ' ')"
  printf 'docsdrift%s' "$(printf '%s\n%s' "$1" "$quote" | sha256sum | cut -c1-12)"
}

new=0 known=0
count="$(jq length "$findings")"
for ((i = 0; i < count; i++)); do
  get() { jq -r ".[$i].$1" "$findings"; }
  fp="$(fingerprint "$(get doc)" "$(get quote)")"
  matches="$(awb search "$fp" --include-closed --all-workspaces --json | jq length)"
  if (( matches > 0 )); then
    known=$((known + 1))
    echo "known   $fp $(get title)"
    continue
  fi
  new=$((new + 1))
  capture="$(get capture)"
  [[ -f "$capture" ]] || capture="$base/$capture"
  if (( !file )); then
    echo "new     $fp $(get title)"
    continue
  fi
  body="$(jq -r --arg fp "$fp" --arg capture "$(basename "$capture")" ".[$i] |
    \"**Doc:** \(.doc):\(.line)\n\n> \(.quote)\n\n**UI shows:** \(.ui_shows)\n\n**Capture:** \(\$capture) (attached), seeded data\n\n**Proposed fix:** \(.fix)\n\nFingerprint: \(\$fp)\"" "$findings")"
  id="$(awb create "$(get title)" --type bug --priority 3 --label docs --label docs-drift --label needs-review \
    "${workspace[@]}" --description "$body" --json | jq -r .id)"
  awb attach add "$id" "$capture" >/dev/null
  echo "filed   $id $fp $(get title)"
done
echo "$new new, $known already known"
