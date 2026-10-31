#!/usr/bin/env bash
# Regenerates the dashboard images in README.md and docs/ from the manifest in
# scripts/docs/shots.json. It seeds a throwaway daemon with invented data under
# an isolated HOME, captures each shot with headless Chrome at a frozen clock,
# and installs the images that changed.
#
#   scripts/docs/screenshots.sh                  capture and install
#   scripts/docs/screenshots.sh --dry-run        capture and report only
#   scripts/docs/screenshots.sh --only 'learn-'  capture matching shots
#
# Needs Chrome or Chromium and the Noto Sans, DejaVu Sans Mono and Noto Color Emoji
# fonts. Output and the contact sheet land in docshots-out/ (gitignored). Any
# missing page, tab or element fails the run before anything is installed.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
install=(--install)
only=()
while (( $# )); do
  case "$1" in
    --dry-run) install=() ;;
    --only) only=(--only "$2"); shift ;;
    *) echo "usage: $0 [--dry-run] [--only REGEXP]" >&2; exit 2 ;;
  esac
  shift
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cd "$root"
(cd scripts/e2e/drive && go build -o "$work/docshots" ./docshots)
# The first build recompiles everything above package time and takes minutes;
# later ones reuse the build cache while the frozen time stays the same.
"$work/docshots" -manifest scripts/docs/shots.json -write-overlay "$work"
go test -c -overlay "$work/overlay.json" -o "$work/agentd.test" ./pkg/claude/agentd/

# Nothing below may see the operator's home: the server, Chrome and the driver
# all run with HOME inside the throwaway directory.
export DOCSHOTS_REAL_HOME="$HOME"
export HOME="$work/home" TZ=UTC LC_ALL=C.UTF-8
mkdir -p "$HOME"
"$work/docshots" -root "$root" -manifest scripts/docs/shots.json \
  -server "$work/agentd.test" -server-dir pkg/claude/agentd \
  -out "$root/docshots-out" "${install[@]}" "${only[@]}"
