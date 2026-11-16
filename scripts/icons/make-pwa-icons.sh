#!/usr/bin/env bash
# Regenerates the dashboard's web app icons in pkg/claude/agentd/dashboard/pwa
# (served by dashboard_pwa.go) from the dashboard icon
# (pkg/claude/agentd/dashboard/tclaude-icon.svg):
#   icon-192.png, icon-512.png  "any" icons: the orange disc on transparent
#   icon-maskable-512.png       Android's adaptive icon: full-bleed orange,
#                               the white glyph as large as the 80%
#                               safe-zone circle allows
#   apple-touch-icon.png        iOS's home-screen icon (180 px), full-bleed
#                               orange too
#
# The full-bleed ones are orange to the edge because a launcher crops the
# square to its own shape (circle, squircle, rounded square): any other
# background shows as a ring around the logo. iOS fills transparency with
# black, so its icon is full-bleed as well.
#
# The outputs are committed; nothing runs this at build time. It needs
# Google Chrome (headless rendering) and Go (scripts/icons/pack downscales).
# Usage: scripts/icons/make-pwa-icons.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
svg="$root/pkg/claude/agentd/dashboard/tclaude-icon.svg"
out="$root/pkg/claude/agentd/dashboard/pwa"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Chrome's crashpad aborts when ~/.config is unwritable (agent sandboxes).
export XDG_CONFIG_HOME="$work/xdg" XDG_CACHE_HOME="$work/xdg" XDG_DATA_HOME="$work/xdg"

# The logo's orange (tclaude-icon.svg's disc).
orange="#ff8c00"

shoot() { # html output (1024 px)
	printf '%s' "$1" >"$work/icon.html"
	google-chrome --headless=new --no-sandbox --disable-gpu --hide-scrollbars \
		--default-background-color=00000000 --force-device-scale-factor=1 \
		--user-data-dir="$work/chrome" --window-size=1024,1024 \
		--screenshot="$2" "file://$work/icon.html" >/dev/null 2>&1
}

# Render large once and downscale (small Chrome windows came back truncated;
# see make-launcher-icons.sh).
#
# The "any" icons are the logo as drawn: the orange disc filling the square.
shoot "<!doctype html><style>html,body{margin:0;background:transparent}img{display:block;width:1024px;height:1024px}</style><img src=\"file://$svg\">" "$work/any.png"

# The full-bleed icons draw the logo on its own orange, so the disc
# disappears into the background and only the white glyph shows. The glyph
# is as large as the maskable safe zone allows: a launcher may crop to any
# shape that contains the centred circle of 40% radius, so the glyph's
# smallest enclosing circle (centre (11, 11.94), radius 9.19 in the SVG's
# 22-unit viewBox) is placed on that circle, with a hair of margin for the
# antialiased edge (9.3 units = 409.6 px, 44.04 px a unit).
unit=44.04
left=$(awk "BEGIN{print 512 - 11 * $unit}")
top=$(awk "BEGIN{print 512 - 11.94 * $unit}")
size=$(awk "BEGIN{print 22 * $unit}")
shoot "<!doctype html><style>html,body{margin:0;width:1024px;height:1024px;overflow:hidden;background:$orange}img{position:absolute;left:${left}px;top:${top}px;width:${size}px;height:${size}px}</style><img src=\"file://$svg\">" "$work/bleed.png"

pack() { (cd "$root" && go run ./scripts/icons/pack -png "$@"); }
pack 192 "$work/any.png" "$out/icon-192.png"
pack 512 "$work/any.png" "$out/icon-512.png"
pack 512 "$work/bleed.png" "$out/icon-maskable-512.png"
pack 180 "$work/bleed.png" "$out/apple-touch-icon.png"
echo "wrote $out/icon-192.png, icon-512.png, icon-maskable-512.png, apple-touch-icon.png"
