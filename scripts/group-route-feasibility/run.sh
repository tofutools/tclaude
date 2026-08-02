#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"
tmp_root="${RUNNER_TEMP:-}"
cleanup_tmp=false
if [[ -z "$tmp_root" ]]; then
  tmp_root="$(mktemp -d)"
  cleanup_tmp=true
fi
if [[ "$cleanup_tmp" == true ]]; then
  trap 'rm -rf "$tmp_root"' EXIT
fi

binary="$tmp_root/tclaude-group-route-feasibility"
go build -trimpath -o "$binary" ./scripts/group-route-feasibility

case "$(uname -s)" in
  Linux)
    exec "$binary" --mode linux
    ;;
  Darwin)
    exec "$binary" --mode darwin
    ;;
  *)
    echo "group-route-feasibility: unsupported host $(uname -s)" >&2
    exit 1
    ;;
esac
