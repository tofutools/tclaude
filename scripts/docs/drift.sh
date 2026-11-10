#!/usr/bin/env bash
# Walks the dashboard on a seeded throwaway server and records what each page in
# scripts/docs/tour.json shows, as <name>.txt and <name>.png in docs-drift-out/tour,
# with an index.json. It reads only: nothing touches docs/, the operator's daemon or
# the operator's dashboard. .claude/skills/docs-drift compares the result with docs/.
#
#   scripts/docs/drift.sh                 record the tour
#   scripts/docs/drift.sh --images        also report docs images that no longer match
#
# Needs Chrome or Chromium, and the seeded server from scripts/docs/screenshots.sh.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
images=()
case "${1:-}" in
  "") ;;
  --images) images=(1) ;;
  *) echo "usage: $0 [--images]" >&2; exit 2 ;;
esac

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cd "$root"
(cd scripts/e2e/drive && go build -o "$work/docshots" ./docshots && go build -o "$work/docdrift" ./docdrift)
# The server refuses to run without the frozen clock of the screenshot build.
"$work/docshots" -root "$root" -manifest scripts/docs/shots.json -write-overlay "$work"
go test -c -overlay "$work/overlay.json" -o "$work/agentd.test" ./pkg/claude/agentd/
frozen="$(jq -r .frozenTime scripts/docs/shots.json)"

# Nothing below may see the operator's machine, because captures go on tickets.
# Config shows PATH lookups and data paths, so the tour runs on a bare environment
# and under /tmp, whose path names nobody. The driver refuses a stop that still
# shows the user, the host or the home directory.
chrome="${TCLAUDE_DOCSHOTS_CHROME:-$(command -v google-chrome chromium chromium-browser | head -1 || true)}"
sandbox="$(TMPDIR=/tmp mktemp -d -t docdrift.XXXXXX)"
trap 'rm -rf "$work" "$sandbox"' EXIT
mkdir -p "$sandbox/home" "$sandbox/tmp"
rm -rf docs-drift-out/tour
env -i HOME="$sandbox/home" TMPDIR="$sandbox/tmp" PATH=/usr/bin:/bin TZ=UTC LC_ALL=C.UTF-8 \
  TCLAUDE_DOCSHOTS_CHROME="$chrome" \
  "$work/docdrift" -tour scripts/docs/tour.json -server "$work/agentd.test" \
  -server-dir pkg/claude/agentd -out "$root/docs-drift-out/tour" -frozen-time "$frozen" \
  -host-words "$HOME"

if (( ${#images[@]} )); then
  scripts/docs/screenshots.sh --dry-run | tee docs-drift-out/images.txt
fi
