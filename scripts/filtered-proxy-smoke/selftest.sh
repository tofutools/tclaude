#!/usr/bin/env bash
# Proves the evidence checker actually refuses the things it claims to refuse.
#
# run.sh executes this BEFORE any smoke, so a broken checker fails the run
# immediately instead of silently passing every flow. That ordering is the
# point: an evidence guard proven once at review time can rot, while one that
# proves itself on every run cannot.
#
# It needs no sandbox, no network and no fixtures, so it is safe to run locally
# — unlike the smokes themselves, which must only ever run in CI.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/evidence.sh
source "$here/lib/evidence.sh"

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

if [[ "$failures" -ne 0 ]]; then
  echo "evidence checker selftest FAILED; refusing to trust any smoke result"
  exit 1
fi
echo "evidence checker selftest: ok"
