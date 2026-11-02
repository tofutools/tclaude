#!/usr/bin/env bash
# Installs the pi harness for CI from scripts/ci/pi, whose package-lock.json
# pins pi's whole dependency tree. A bare `npm install --global` pinned only pi
# itself, so a dependency published mid-run (a pi-tui release that 404ed for a
# while) broke every PR's required checks.
#
# It installs with `npm ci` under npm's global prefix and links `pi` into the
# global bin, so pi resolves where a global install would: inside the
# read-only OS surface the sandbox smokes bind (/usr or /opt). A failed install
# is retried once, for a registry that answers 404 while a publish settles.
#
#   scripts/ci/install-pi.sh
set -euo pipefail

src="$(cd "$(dirname "${BASH_SOURCE[0]}")/pi" && pwd)"
prefix="$(npm prefix --global)"
dest="$prefix/lib/tclaude-ci-pi"

rm -rf "$dest"
mkdir -p "$dest"
cp "$src/package.json" "$src/package-lock.json" "$dest/"

install() { npm ci --prefix "$dest" --no-audit --no-fund; }
if ! install; then
  echo "pi install failed; retrying once in 30s" >&2
  sleep 30
  install
fi

# Windows is not a CI target for pi; bin is where npm links global commands.
bin="$prefix/bin"
mkdir -p "$bin"
ln -sf "$dest/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js" "$bin/pi"
pi --version </dev/null
