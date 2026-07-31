#!/usr/bin/env bash
# Proves the shared evidence checker and the manifest drift guards actually
# refuse the things they claim to refuse.
#
# Every shard's run.sh executes this BEFORE any smoke, so a broken checker fails
# the run immediately instead of silently passing every flow. That ordering is
# the point: an evidence guard proven once at review time can rot, while one
# that proves itself on every run cannot. It lives beside the code it proves,
# and both are shared, so the discipline cannot differ between shards.
#
# It needs no sandbox, no network and no fixtures, so it is safe to run locally
# — unlike the smokes themselves, which must only ever run in CI.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "$here/common.sh"
# shellcheck source=evidence.sh
source "$here/evidence.sh"
# shellcheck source=driver.sh
source "$here/driver.sh"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failures=0

# expect_verdict WANT(pass|refuse) CASE LOG_CONTENT TEST_NAME...
expect_verdict() {
  local want="$1" name="$2" content="$3"
  shift 3
  local log="$work/$name.log"
  printf '%s' "$content" > "$log"
  local got=pass
  require_passed_tests "$log" "$@" > "$work/$name.out" 2>&1 || got=refuse
  if [[ "$got" != "$want" ]]; then
    printf 'selftest FAIL: %s — wanted %s, got %s\n' "$name" "$want" "$got"
    sed 's/^/    /' "$work/$name.out"
    failures=1
  fi
}

# The green case. Without this the suite could be satisfied by a checker that
# refuses everything, which would be just as broken and far more annoying.
expect_verdict pass green '=== RUN   TestSmokeA
--- PASS: TestSmokeA (1.00s)
=== RUN   TestSmokeB
--- PASS: TestSmokeB (2.00s)
PASS
' TestSmokeA TestSmokeB

# The four ways a smoke stops being evidence while `go test` still exits 0.
expect_verdict refuse skipped '=== RUN   TestSmokeA
--- SKIP: TestSmokeA (0.00s)
PASS
' TestSmokeA

expect_verdict refuse renamed '=== RUN   TestSmokeRenamed
--- PASS: TestSmokeRenamed (1.00s)
PASS
' TestSmokeA

expect_verdict refuse no-tests-to-run 'testing: warning: no tests to run
PASS
ok  	example/pkg	0.00s
' TestSmokeA

expect_verdict refuse empty-log '' TestSmokeA

# A subtest pass is indented, so it must not satisfy its parent: a parent that
# fails after one green subtest would otherwise read as evidence.
expect_verdict refuse subtest-only '=== RUN   TestSmokeA/case
    --- PASS: TestSmokeA/case (0.10s)
--- FAIL: TestSmokeA (0.20s)
FAIL
' TestSmokeA

# Prefix collision: TestSmoke must not be satisfied by TestSmokeExtra.
expect_verdict refuse prefix-collision '--- PASS: TestSmokeExtra (1.00s)
PASS
' TestSmoke

# Partial coverage: one green name never speaks for the others.
expect_verdict refuse partial '--- PASS: TestSmokeA (1.00s)
PASS
' TestSmokeA TestSmokeB

# Declaring nothing must be an error, not a free pass.
expect_verdict refuse empty-requirement '--- PASS: TestSmokeA (1.00s)
PASS
'

# A failure must be refused even though the name appears in the log.
expect_verdict refuse failed '--- FAIL: TestSmokeA (1.00s)
FAIL
' TestSmokeA

# --- The manifest drift guards -----------------------------------------------
#
# The evidence checker only judges a flow that RAN. The other half of the
# discipline decides which flows must run and what they must claim, and it can
# rot in exactly the same way — a shard whose manifest silently stopped naming a
# flow would pass every check above while proving nothing.
#
# expect_manifest WANT(pass|refuse) CASE MANIFEST_CONTENT FLOW_NAME...
expect_manifest() {
  local want="$1" name="$2" content="$3"
  shift 3
  local dir="$work/manifest-$name"
  mkdir -p "$dir/flows"
  printf '%s' "$content" > "$dir/manifest.txt"
  local flow
  for flow in "$@"; do
    printf 'flow::run() { :; }\n' > "$dir/flows/$flow.sh"
  done
  local got=pass
  smoke::load_manifest "$dir/manifest.txt" "$dir/flows" \
    > "$dir/out" 2>&1 || got=refuse
  if [[ "$got" != "$want" ]]; then
    printf 'selftest FAIL: manifest-%s — wanted %s, got %s\n' "$name" "$want" "$got"
    sed 's/^/    /' "$dir/out"
    failures=1
  fi
}

# The green case, including a comment line and a final line with no trailing
# newline — the shape that would otherwise be dropped silently at EOF.
expect_manifest pass green '# comment
10-alpha TestAlpha
10-alpha TestAlphaTwo

20-beta TestBeta' 10-alpha 20-beta

# A flow with no manifest entry is a smoke that cannot fail.
expect_manifest refuse flow-without-evidence '10-alpha TestAlpha
' 10-alpha 20-beta

# A manifest entry with no flow is evidence claimed for a smoke that no longer
# runs.
expect_manifest refuse evidence-without-flow '10-alpha TestAlpha
20-beta TestBeta
' 10-alpha

# A line naming a flow but no test would let that flow pass by running nothing.
expect_manifest refuse line-without-test '10-alpha
' 10-alpha

# A shard with no flows at all must not report a clean validation.
expect_manifest refuse no-flows '10-alpha TestAlpha
'

# An empty manifest names no evidence for the flows that exist.
expect_manifest refuse empty-manifest '' 10-alpha

if [[ "$failures" -ne 0 ]]; then
  echo "smoke evidence selftest FAILED; refusing to trust any smoke result"
  exit 1
fi
echo "smoke evidence selftest: ok (evidence checker + manifest drift guards)"
