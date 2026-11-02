#!/usr/bin/env bash
# Type-check (go vet) every package and its tests as GOOS=darwin from a Linux
# runner. PR CI runs the macOS jobs only when the change classifier says the
# diff is platform code (scripts/ci/classify-changes.py), so this is the cheap
# net under that: a darwin compile break anywhere still fails the PR.
#
# Cross-checking needs CGO_ENABLED=0, which drops cgo files. fyne.io/systray's
# darwin backend is cgo-only, so without it the package (and agentd, which
# imports it) would not type-check. A throwaway -modfile replaces the module
# with a copy whose darwin file is a stub generated from its own func
# signatures: our code is checked against the real darwin API, only the C
# bodies are gone. go.mod and go.sum in the checkout are untouched. Any other cgo-only dependency fails
# here loudly; stub it the same way rather than skipping packages.
set -euo pipefail
export GOOS=darwin GOARCH=arm64 CGO_ENABLED=0

work="$(mktemp -d "${TMPDIR:-/tmp}/darwin-vet.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT

systray_dir="$(go list -m -f '{{.Dir}}' fyne.io/systray)"
cp -R "$systray_dir" "$work/systray"
chmod -R u+w "$work/systray"
python3 - "$work/systray/systray_darwin.go" <<'PY'
import pathlib, re, sys
src = pathlib.Path(sys.argv[1])
text = src.read_text()
build = re.search(r"^//go:build .*$", text, re.MULTILINE)
out = [build.group(0) if build else "", "", "package systray", ""]
stubs = 0
for line in text.splitlines():
    m = re.match(r"^(func .*\))\s*(\w+|\([^)]*\))?\s*\{$", line)
    if not m or "C." in line:
        continue
    result = m.group(2)
    if result is None:
        body = "{}"
    elif result == "error":
        body = "{ return nil }"
    else:
        sys.exit(f"darwin-vet: cannot stub {line!r}; extend the generator")
    out.append(f"{m.group(1)} {result or ''} {body}".replace("  ", " "))
    stubs += 1
if stubs < 10:
    sys.exit(f"darwin-vet: only {stubs} stubs generated from {src}; layout changed?")
src.write_text("\n".join(out) + "\n")
PY
cp go.mod "$work/go.mod"
cp go.sum "$work/go.sum"
go mod edit -modfile="$work/go.mod" -replace="fyne.io/systray=$work/systray"

pkgs="$(go list -modfile="$work/go.mod" ./...)"
if [ -z "$pkgs" ]; then
  echo "darwin-vet: no packages listed" >&2
  exit 1
fi
# shellcheck disable=SC2086
go vet -modfile="$work/go.mod" $pkgs
