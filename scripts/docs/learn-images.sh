#!/usr/bin/env bash
# Regenerates every image used by the Learn tutorials (docs/learn/) and
# installs them into docs/assets/. The images come from gated Go tests that
# render real tclaude views from fake data (no real names, paths, hosts or
# costs) and screenshot them with a local headless Chrome:
#
#   pkg/claude/agentd   TestLearnShots  dashboard (dashsnap)
#   pkg/claude/session  TestLearnShots  tclaude session watch
#   pkg/claude/conv     TestLearnShots  tclaude conv watch
#
# Usage: scripts/docs/learn-images.sh
# Needs Google Chrome or Chromium. Uses oxipng or optipng when installed.
# Look at every image before committing it.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
out="$root/dashsnap-out/learn-images"
rm -rf "$out"

# Chrome's crashpad ignores --user-data-dir and aborts when ~/.config is not
# writable (agent sandboxes), so point the XDG dirs at a throwaway directory.
xdg="$(mktemp -d)"
trap 'rm -rf "$xdg"' EXIT
export XDG_CONFIG_HOME="$xdg/config" XDG_CACHE_HOME="$xdg/cache" XDG_DATA_HOME="$xdg/data"
export TCLAUDE_DOCSHOTS=1
# UTC keeps the timestamps in the images free of the local timezone.
export TZ=UTC

cd "$root"
go test ./pkg/claude/agentd/ ./pkg/claude/session/ ./pkg/claude/conv/ \
  -run '^TestLearnShots$' -count=1 -timeout 600s

shopt -s nullglob
pngs=("$out"/learn-*.png)
if (( ${#pngs[@]} != 5 )); then
  echo "expected 5 images in $out, got ${#pngs[@]}: is Chrome installed? (the generators skip without it)" >&2
  exit 1
fi
for png in "${pngs[@]}"; do
  if command -v oxipng >/dev/null; then
    oxipng -q -o 4 --strip safe "$png"
  elif command -v optipng >/dev/null; then
    optipng -quiet -o2 "$png"
  fi
  cp "$png" "$root/docs/assets/"
  printf '%6s KB  docs/assets/%s\n' "$(( $(wc -c <"$png") / 1024 ))" "$(basename "$png")"
done
