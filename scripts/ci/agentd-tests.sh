#!/usr/bin/env bash
# Keep mutable fixture globals serial within each process, while running
# several processes concurrently on the same runner. Plain go test remains
# unchanged.
set -euo pipefail

# Each side runs the tests matching its prefix regex minus every earlier
# side's; the final side runs the exact complement of all of them, including
# examples and fuzz seeds, so new tests need no manually maintained list.
# Balanced on measured macOS per-test timings (the slower runner).
side_names=(federation dashboard other)
side_patterns=(
  '^Test(Federation|Peer|Pi|Reincarnate|Open|Codex)'
  '^Test(Dashboard|Spawn|Clone|Operator|RealGit|Morph|Copilot|Create)'
)
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
# Compile (and vet) the test binary once before fanning out. Concurrent
# `go test` processes do not share in-flight work: each compiled the whole
# agentd package and its deps itself, which doubled the build phase on the
# small macOS runner. Afterwards every side reuses the cached compile and vet
# results and only relinks (go does not cache linked test binaries).
prebuild=(-run '^$')
if [ "${TCLAUDE_AGENTD_TEST_COVER:-}" = 1 ]; then prebuild+=(-cover); fi
if ! "${runner[@]}" "${prebuild[@]}" "$@" ./pkg/claude/agentd > "$logs/build.log" 2>&1; then
  cat "$logs/build.log"
  echo "agentd-tests: building (or starting) the test binary failed" >&2
  exit 1
fi

pids=()
earlier=""
for i in "${!side_names[@]}"; do
  side=${side_names[$i]}
  selection=()
  if [ "$i" -lt "${#side_patterns[@]}" ]; then selection=(-run "${side_patterns[$i]}"); fi
  if [ -n "$earlier" ]; then selection+=(-skip "$earlier"); fi
  if [ "${#selection[@]}" -eq 0 ]; then
    echo "agentd-tests: side $side selects nothing" >&2
    exit 2
  fi
  run_side "$side" "$@" "${selection[@]}" > "$logs/$side.json" 2>&1 &
  pids+=("$!")
  if [ "$i" -lt "${#side_patterns[@]}" ]; then
    earlier="${earlier:+$earlier|}${side_patterns[$i]}"
  fi
done
report=()
failed=0
for i in "${!side_names[@]}"; do
  status=0
  wait "${pids[$i]}" || status=$?
  if [ "$status" -ne 0 ]; then failed=1; fi
  report+=("$logs/${side_names[$i]}.json" "$status")
done

# Report only after every run completes: assertions and failing test names
# are grouped by side, never interleaved. Preserve build/setup failures too.
python3 scripts/ci/agentd-test-report.py "${report[@]}"
if [ "$failed" -ne 0 ]; then exit 1; fi
