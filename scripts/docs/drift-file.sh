#!/usr/bin/env bash
# Files docs-drift findings as awb tickets, once each. A quote must be the doc's own
# words. The fingerprint comes from the doc page and the whole sentence around the
# quote, so neither a moved line nor a shorter quote makes a new finding, and it
# goes in the ticket. A finding whose fingerprint is on any ticket, closed ones
# included, is skipped, so closing a ticket as "not drift" also silences it.
#
#   scripts/docs/drift-file.sh findings.json                 show what would be filed
#   scripts/docs/drift-file.sh --file -w tcl findings.json   file it
#
# findings.json is an array of objects with these fields: title, doc, line, quote,
# ui_shows, capture (a tour image) and fix (the proposed wording).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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

# sentence prints the sentence of a doc that holds the quote, in lower case on one
# line, and fails if the doc does not hold it. Docs are hard-wrapped, so it matches
# across lines; a blank line or a table cell ends a sentence too.
sentence() { # sentence <doc> <quote>
  QUOTE="$2" perl -0777 -ne '
    my $q = lc $ENV{QUOTE}; $q =~ s/\s+/ /g; $q =~ s/^ | $//g;
    my $d = lc $_; $d =~ s/\n[ \t]*\n/\x00/g; $d =~ s/\s+/ /g;
    my $i = index($d, $q); exit 1 if $i < 0;
    my $l = substr($d, 0, $i); $l =~ s/.*(?:[.!?] |\||\x00)//s;
    my $r = substr($d, $i + length $q); $r =~ s/(?:[.!?](?= )|\||\x00).*//s;
    print $l . $q . $r;' "$root/$1"
}

fingerprint() { # fingerprint <doc> <sentence>
  local hash
  if command -v sha256sum >/dev/null; then
    hash="$(printf '%s\n%s' "$1" "$2" | sha256sum)"
  else
    hash="$(printf '%s\n%s' "$1" "$2" | shasum -a 256)"
  fi
  printf 'docsdrift%s' "${hash:0:12}"
}

new=0 known=0 unattached=0
count="$(jq length "$findings")"
for ((i = 0; i < count; i++)); do
  get() { jq -r ".[$i].$1" "$findings"; }
  if ! said="$(sentence "$(get doc)" "$(get quote)")"; then
    echo "not the doc's own words, nothing more filed: $(get title)" >&2
    exit 1
  fi
  fp="$(fingerprint "$(get doc)" "$said")"
  [[ "$fp" =~ ^docsdrift[0-9a-f]{12}$ ]] || { echo "bad fingerprint: $fp" >&2; exit 1; }
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
  echo "filed   $id $fp $(get title)"
  # The ticket exists now, so a later run calls it known and will not retry this.
  if ! awb attach add "$id" "$capture" >/dev/null; then
    echo "attach $capture to $id by hand" >&2
    unattached=1
  fi
done
echo "$new new, $known already known"
exit "$unattached"
