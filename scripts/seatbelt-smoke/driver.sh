#!/usr/bin/env bash
# Repository-owned driver for the real Darwin Seatbelt smokes.
#
# CI delegates the complete named-test sequence here so adding another smoke is
# a reviewed repository change, not another workflow edit. Every command is
# followed by an exact top-level PASS check: a skip, rename, build-tag mismatch,
# or zero-test success is a hard failure rather than absent evidence.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

: "${RUNNER_TEMP:?RUNNER_TEMP must name the CI artifact directory}"
if [[ -z "${GITHUB_STEP_SUMMARY:-}" ]]; then
  GITHUB_STEP_SUMMARY="$RUNNER_TEMP/seatbelt-smoke-summary.md"
  : > "$GITHUB_STEP_SUMMARY"
  trap '[[ ! -s "$GITHUB_STEP_SUMMARY" ]] || cat "$GITHUB_STEP_SUMMARY" >&2' EXIT
fi

smoke_log="$RUNNER_TEMP/tclaude-layer-darwin-smoke.log"
set +e
go test ./pkg/claude/session -run '^TestTclaudeLayerDarwinSmoke$' -count=1 -v -timeout=120s |
  tee "$smoke_log"
pipeline_status=("${PIPESTATUS[@]}")
set -e
if [[ "${pipeline_status[0]}" -ne 0 || "${pipeline_status[1]}" -ne 0 ]] ||
  ! grep -q '^--- PASS: TestTclaudeLayerDarwinSmoke ' "$smoke_log"; then
  {
    echo "### Sandbox v2 Seatbelt smoke did not complete"
    echo
    echo "The test command exited successfully without reporting that the named real smoke passed."
    echo "The test name, build constraints, or smoke gate likely changed; see the test log."
  } >> "$GITHUB_STEP_SUMMARY"
  echo "::error::TestTclaudeLayerDarwinSmoke did not report an explicit pass"
  exit 1
fi

proxy_floor_log="$RUNNER_TEMP/seatbelt-proxy-floor-smoke.log"
set +e
go test ./pkg/claude/session -run '^TestSeatbeltProxyFloorSmoke$' -count=1 -v -timeout=120s |
  tee "$proxy_floor_log"
pipeline_status=("${PIPESTATUS[@]}")
set -e
if [[ "${pipeline_status[0]}" -ne 0 || "${pipeline_status[1]}" -ne 0 ]] ||
  ! grep -q '^--- PASS: TestSeatbeltProxyFloorSmoke ' "$proxy_floor_log"; then
  {
    echo "### Seatbelt proxy-floor smoke did not complete"
    echo
    echo "The real Darwin proxy-floor smoke did not report an explicit pass."
    echo "A skip, missing/renamed test, build-tag mismatch, or zero-test success is a hard failure."
  } >> "$GITHUB_STEP_SUMMARY"
  echo "::error::TestSeatbeltProxyFloorSmoke did not report an explicit pass"
  exit 1
fi

refusal_log="$RUNNER_TEMP/stacked-seatbelt-refusal.log"
set +e
go test ./pkg/claude/session -run '^TestStackedSandboxDarwinRefusal$' -count=1 -v -timeout=30s |
  tee "$refusal_log"
pipeline_status=("${PIPESTATUS[@]}")
set -e
if [[ "${pipeline_status[0]}" -ne 0 || "${pipeline_status[1]}" -ne 0 ]] ||
  ! grep -q '^--- PASS: TestStackedSandboxDarwinRefusal ' "$refusal_log"; then
  echo "::error::TestStackedSandboxDarwinRefusal did not report an explicit pass"
  exit 1
fi

printf '%s\n' \
  "Seatbelt smoke evidence complete:" \
  "--- PASS: TestTclaudeLayerDarwinSmoke" \
  "--- PASS: TestSeatbeltProxyFloorSmoke" \
  "--- PASS: TestStackedSandboxDarwinRefusal"
