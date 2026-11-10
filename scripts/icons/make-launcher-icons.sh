#!/usr/bin/env bash
# Regenerates the app launcher icons in pkg/launcher/assets from the
# dashboard icon (pkg/claude/agentd/dashboard/tclaude-icon.svg):
#   tclaude.svg   the Linux hicolor scalable icon (a copy)
#   tclaude.png   a 256x256 PNG for Linux fallbacks
#   tclaude.icns  the macOS bundle icon (PNG-backed ic07..ic14 entries)
#
# The outputs are committed; nothing runs this at build time. It needs
# Google Chrome (headless rendering) and python3 (packing the .icns).
# Usage: scripts/icons/make-launcher-icons.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
svg="$root/pkg/claude/agentd/dashboard/tclaude-icon.svg"
out="$root/pkg/launcher/assets"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Chrome's crashpad aborts when ~/.config is unwritable (agent sandboxes).
export XDG_CONFIG_HOME="$work/xdg" XDG_CACHE_HOME="$work/xdg" XDG_DATA_HOME="$work/xdg"

cp "$svg" "$out/tclaude.svg"

# macOS icons leave a margin around the artwork (Apple's grid puts the body
# at about 824 of 1024 px).
render() { # size pad-percent output
	local size=$1 pad=$2 png=$3
	cat >"$work/icon.html" <<EOF
<!doctype html><html><head><style>
html,body{margin:0;background:transparent}
img{display:block;width:${size}px;height:${size}px;box-sizing:border-box;padding:calc(${size}px * ${pad} / 100)}
</style></head><body><img src="file://$svg"></body></html>
EOF
	google-chrome --headless=new --no-sandbox --disable-gpu --hide-scrollbars \
		--default-background-color=00000000 --force-device-scale-factor=1 \
		--user-data-dir="$work/chrome" --window-size="$size,$size" \
		--screenshot="$png" "file://$work/icon.html" >/dev/null 2>&1
}

render 256 0 "$out/tclaude.png"
for s in 16 32 64 128 256 512 1024; do
	render "$s" 10 "$work/$s.png"
done

python3 -I - "$work" "$out/tclaude.icns" <<'EOF'
import struct, sys
work, dst = sys.argv[1], sys.argv[2]
# OSType -> pixel size of its PNG payload.
entries = [("ic07", 128), ("ic08", 256), ("ic09", 512), ("ic10", 1024),
           ("ic11", 32), ("ic12", 64), ("ic13", 256), ("ic14", 512)]
body = b""
for ostype, size in entries:
    data = open(f"{work}/{size}.png", "rb").read()
    body += ostype.encode() + struct.pack(">I", len(data) + 8) + data
open(dst, "wb").write(b"icns" + struct.pack(">I", len(body) + 8) + body)
EOF
echo "wrote $out/tclaude.svg, tclaude.png, tclaude.icns"
