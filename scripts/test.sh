#!/usr/bin/env bash
# Run Go tests with a symlinked temp directory so Linux also exercises the
# path aliases macOS commonly supplies (/var -> /private/var).
set -euo pipefail

scratch=$(mktemp -d "${TMPDIR:-/tmp}/tcl.XXXXXX")
cleanup() {
  if ! rm -rf -- "$scratch"; then
    echo "test.sh: could not remove temporary directory $scratch" >&2
  fi
}
trap cleanup EXIT
# TMPDIR may be relative; both the exported path and symlink target must work
# after a test or subprocess changes its working directory.
canonical_scratch=$(cd -- "$scratch" && pwd -P)
scratch=$canonical_scratch
mkdir "$scratch/real"
ln -s "$scratch/real" "$scratch/alias"
export TMPDIR="$scratch/alias"

if [ "$#" -eq 0 ]; then
  set -- ./...
fi
go test "$@"
