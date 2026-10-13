#!/usr/bin/env bash
# Keep mutable fixture globals serial within each process, while using both
# processes concurrently on the same runner. Plain go test remains unchanged.
set -euo pipefail

# Dashboard/federation/operator flows form one readable domain; -skip gives
# the other process its exact complement, including examples and fuzz seeds.
domain='^Test(Dashboard|Federation|Peer|Operator)'
for arg in "$@"; do
  case "$arg" in
    -run*|-skip*|-list*|-coverprofile*)
      echo "agentd-tests: selection/profile flags are owned by this runner" >&2
      exit 2 ;;
  esac
done

logs="${TCLAUDE_AGENTD_TEST_LOG_DIR:-}"
if [ -z "$logs" ]; then
  logs=$(mktemp -d "${TMPDIR:-/tmp}/tcl-agentd-tests.XXXXXX")
  trap 'rm -rf -- "$logs"' EXIT
else
  mkdir -p "$logs"
fi
runner=(go test)
if [ "${RUNNER_OS:-$(uname -s)}" = Linux ]; then runner=(scripts/test.sh); fi

run_side() {
  local side=$1
  shift
  # Bash 3.2 (macOS /bin/bash) treats an empty array as unset under -u.
  if [ "${TCLAUDE_AGENTD_TEST_COVER:-}" = 1 ]; then
    "${runner[@]}" -json "-coverprofile=$logs/$side.cover" "$@" ./pkg/claude/agentd
  else
    "${runner[@]}" -json "$@" ./pkg/claude/agentd
  fi
}
run_side domain "$@" -run "$domain" > "$logs/domain.json" 2>&1 &
domain_pid=$!
run_side other "$@" -skip "$domain" > "$logs/other.json" 2>&1 &
other_pid=$!
domain_status=0
other_status=0
wait "$domain_pid" || domain_status=$?
wait "$other_pid" || other_status=$?

# Report only after both runs complete: assertions and failing test names are
# grouped by side, never interleaved. Preserve build/setup failures as well.
python3 scripts/ci/agentd-test-report.py "$logs/domain.json" "$domain_status" "$logs/other.json" "$other_status"
if [ "$domain_status" -ne 0 ] || [ "$other_status" -ne 0 ]; then exit 1; fi
