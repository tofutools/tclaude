#!/usr/bin/env bash
# Type-check (go vet) every package and its tests as GOOS=darwin from a Linux
# runner. PR CI runs the macOS jobs only when the change classifier says the
# diff is platform code (scripts/ci/classify-changes.py), so this is the cheap
# net under that: a darwin compile break anywhere still fails the PR.
#
# Packages whose (test) closure reaches cgo-only dependencies cannot be
# type-checked without a darwin C toolchain and are listed as skipped; the
# classifier's per-file rule and release.yml's macOS jobs cover them.
set -euo pipefail
export GOOS=darwin GOARCH=arm64 CGO_ENABLED=0
cgo_only='^fyne\.io/systray$'

# One `go list` over every package's test variant: print the package, then
# whether anything in its closure is cgo-only.
skip="$(go list -test -deps=false -f '{{.ImportPath}} {{join .Deps " "}}' ./... |
  awk -v re="$cgo_only" '{ for (i = 2; i <= NF; i++) if ($i ~ re) { print $1; break } }' |
  sed 's/_test$//; s/\.test$//' | LC_ALL=C sort -u)"
pkgs="$(go list ./... | LC_ALL=C sort -u)"
if [ -n "$skip" ]; then
  echo "darwin-vet: skipping (cgo-only dependency): $(echo "$skip" | tr '\n' ' ')"
  pkgs="$(LC_ALL=C comm -23 <(echo "$pkgs") <(echo "$skip"))"
fi
if [ -z "$pkgs" ]; then
  echo "darwin-vet: nothing left to check" >&2
  exit 1
fi
# shellcheck disable=SC2086
go vet $pkgs
