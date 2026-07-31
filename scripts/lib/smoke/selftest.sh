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

# Several names on ONE line must ALL be required. The refusal case below is the
# half that matters: if the second name were dropped, this log — which passes the
# first and fails the second — would be accepted as evidence.
expect_manifest pass multi-name-green '10-alpha TestAlpha TestAlphaTwo
' 10-alpha
# ...and the loaded set is inspected, not just the exit status: a parser that
# accepted the line while recording one name would satisfy the case above.
if [[ "${SMOKE_REQUIRED_BY_FLOW[10-alpha]:-}" != "TestAlpha TestAlphaTwo " ]]; then
  printf 'selftest FAIL: manifest-multi-name-green recorded %q, wanted both names\n' \
    "${SMOKE_REQUIRED_BY_FLOW[10-alpha]:-}"
  failures=1
fi
expect_verdict refuse multi-name-partial '--- PASS: TestAlpha (1.00s)
--- FAIL: TestAlphaTwo (1.00s)
FAIL
' TestAlpha TestAlphaTwo

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

# --- The shard-map guards -----------------------------------------------------
#
# Splitting a shard's flows across CI jobs adds a third way to prove nothing,
# which neither set of guards above can see: a flow that has manifest evidence,
# has a flow file, passes every drift check — and is assigned to no job, so it
# never runs anywhere while every job stays green. The union check refuses that,
# and this section is what proves the union check actually refuses it.
#
# expect_shards WANT(pass|refuse) CASE MANIFEST_CONTENT SHARDS_CONTENT FLOW_NAME...
expect_shards() {
  local want="$1" name="$2" manifest_content="$3" shards_content="$4"
  shift 4
  local dir="$work/shards-$name"
  mkdir -p "$dir/flows"
  printf '%s' "$manifest_content" > "$dir/manifest.txt"
  printf '%s' "$shards_content" > "$dir/shards.txt"
  local flow
  for flow in "$@"; do
    printf 'flow::run() { :; }\n' > "$dir/flows/$flow.sh"
  done
  local got=pass
  {
    smoke::load_manifest "$dir/manifest.txt" "$dir/flows" &&
      smoke::load_shards "$dir/shards.txt"
  } > "$dir/out" 2>&1 || got=refuse
  if [[ "$got" != "$want" ]]; then
    printf 'selftest FAIL: shards-%s — wanted %s, got %s\n' "$name" "$want" "$got"
    sed 's/^/    /' "$dir/out"
    failures=1
  fi
}

SHARD_MANIFEST='10-alpha TestAlpha
20-beta TestBeta
30-gamma TestGamma
'
SHARD_FLOWS=(10-alpha 20-beta 30-gamma)

# The green case: every flow assigned, both keys present on every shard.
expect_shards pass green "$SHARD_MANIFEST" '# a comment
one flows      10-alpha 20-beta
one harnesses  claude codex

two flows      30-gamma
two harnesses  opencode' "${SHARD_FLOWS[@]}"

# ...and the parse is inspected, not just the exit status: a reader that
# accepted the line while recording one flow would satisfy the case above and
# then silently run half the shard.
if [[ "${SMOKE_SHARD_FLOWS[one]:-}" != "10-alpha 20-beta " ]]; then
  printf 'selftest FAIL: shards-green recorded flows %q, wanted both\n' \
    "${SMOKE_SHARD_FLOWS[one]:-}"
  failures=1
fi
if [[ "${SMOKE_SHARD_HARNESSES[one]:-}" != "claude codex " ]]; then
  printf 'selftest FAIL: shards-green recorded harnesses %q, wanted both\n' \
    "${SMOKE_SHARD_HARNESSES[one]:-}"
  failures=1
fi

# THE ONE THIS SECTION EXISTS FOR: 30-gamma has evidence and a flow file, but no
# shard runs it. Every other guard in this file passes; only this one refuses.
expect_shards refuse union-gap "$SHARD_MANIFEST" 'one flows      10-alpha 20-beta
one harnesses  claude
' "${SHARD_FLOWS[@]}"

# A shard naming a flow the manifest does not: evidence assigned to a job for a
# smoke that does not exist.
expect_shards refuse unknown-flow "$SHARD_MANIFEST" 'one flows      10-alpha 20-beta 30-gamma 40-delta
one harnesses  claude
' "${SHARD_FLOWS[@]}"

# A shard with no harnesses could not launch a harness smoke, and a shard with
# no flows is a job that runs nothing.
expect_shards refuse no-harnesses "$SHARD_MANIFEST" 'one flows 10-alpha 20-beta 30-gamma
' "${SHARD_FLOWS[@]}"
expect_shards refuse no-flows "$SHARD_MANIFEST" 'one harnesses claude
' "${SHARD_FLOWS[@]}"

# Structural drift: a repeated key silently discards one of the two lists, an
# unknown key is a typo that would drop the line, and a repeated flow inside one
# shard would run the same smoke twice while reading as coverage of two.
expect_shards refuse duplicate-key "$SHARD_MANIFEST" 'one flows      10-alpha 20-beta
one flows      30-gamma
one harnesses  claude
' "${SHARD_FLOWS[@]}"
expect_shards refuse unknown-key "$SHARD_MANIFEST" 'one flowz      10-alpha 20-beta 30-gamma
one harnesses  claude
' "${SHARD_FLOWS[@]}"
expect_shards refuse duplicate-flow "$SHARD_MANIFEST" 'one flows      10-alpha 10-alpha 20-beta 30-gamma
one harnesses  claude
' "${SHARD_FLOWS[@]}"

# Names index associative arrays and are compared through unquoted expansions,
# so a glob or `@` in one would alias every key or expand against the repo root.
expect_shards refuse glob-in-flow "$SHARD_MANIFEST" 'one flows      * 10-alpha 20-beta 30-gamma
one harnesses  claude
' "${SHARD_FLOWS[@]}"
expect_shards refuse glob-in-shard-name "$SHARD_MANIFEST" '@ flows      10-alpha 20-beta 30-gamma
@ harnesses  claude
' "${SHARD_FLOWS[@]}"

# A shard map that declares nothing assigns nothing, and a missing one is not a
# permissive default.
expect_shards refuse empty-map "$SHARD_MANIFEST" '# only a comment
' "${SHARD_FLOWS[@]}"
expect_shards refuse comment-only-list "$SHARD_MANIFEST" 'one flows      # nothing here
one harnesses  claude
' "${SHARD_FLOWS[@]}"

# A shard map that is not there at all: absent must not read as "no constraints".
if smoke::load_shards "$work/shards-green/does-not-exist.txt" \
    > "$work/shards-missing.out" 2>&1; then
  printf 'selftest FAIL: shards-missing-map — wanted refuse, got pass\n'
  failures=1
fi

# Loading a shard map with no manifest behind it would check the union against
# an empty set and pass every time.
if ( SMOKE_REQUIRED_BY_FLOW=(); smoke::load_shards "$work/shards-green/shards.txt" ) \
    > "$work/shards-no-manifest.out" 2>&1; then
  printf 'selftest FAIL: shards-no-manifest — wanted refuse, got pass\n'
  failures=1
fi

# --- Shard selection ----------------------------------------------------------
#
# Selection has its own way to prove nothing: a name that matches no shard must
# fail rather than resolve to an empty flow list, and a name that does match must
# narrow the run to exactly that shard's flows.
smoke::load_manifest "$work/shards-green/manifest.txt" "$work/shards-green/flows" \
  > /dev/null 2>&1
smoke::load_shards "$work/shards-green/shards.txt" > /dev/null 2>&1
if smoke::select_shard nosuchshard > "$work/select-unknown.out" 2>&1; then
  printf 'selftest FAIL: select-unknown — wanted refuse, got pass\n'
  failures=1
fi
if smoke::select_shard two > "$work/select-two.out" 2>&1; then
  selected=""
  for f in "${SMOKE_FLOW_FILES[@]}"; do selected+="$(basename "$f" .sh) "; done
  if [[ "$selected" != "30-gamma " ]]; then
    printf 'selftest FAIL: select-two narrowed to %q, wanted "30-gamma "\n' "$selected"
    failures=1
  fi
  if [[ "${SMOKE_SELECTED_HARNESSES[*]}" != "opencode" ]]; then
    printf 'selftest FAIL: select-two chose harnesses %q, wanted "opencode"\n' \
      "${SMOKE_SELECTED_HARNESSES[*]}"
    failures=1
  fi
else
  printf 'selftest FAIL: select-two — wanted pass, got refuse\n'
  sed 's/^/    /' "$work/select-two.out"
  failures=1
fi

# --- The workflow-matrix guard ------------------------------------------------
#
# The union check proves every flow belongs to a shard. It cannot prove every
# shard is RUN: the list of shards CI invokes lives in a workflow file, and a
# shard added here but not there executes nowhere while every job stays green.
# The shard map loaded above declares shards "one" and "two".
#
# expect_workflow WANT(pass|refuse) CASE WORKFLOW_CONTENT
expect_workflow() {
  local want="$1" name="$2" content="$3"
  local dir="$work/wf-$name"
  mkdir -p "$dir"
  printf '%s' "$content" > "$dir/wf.yml"
  local got=pass
  smoke::require_workflow_shards smoke-job "$dir/wf.yml" \
    > "$dir/out" 2>&1 || got=refuse
  if [[ "$got" != "$want" ]]; then
    printf 'selftest FAIL: wf-%s — wanted %s, got %s\n' "$name" "$want" "$got"
    sed 's/^/    /' "$dir/out"
    failures=1
  fi
}

# The green case: the matrix names exactly the shards the map declares.
expect_workflow pass green 'jobs:
  smoke-job:
    strategy:
      matrix:
        shard: [one, two]
    steps:
      - run: run.sh --shard ${{ matrix.shard }}
  other-job:
    steps:
      - run: something-else
'

# THE ONE THIS SECTION EXISTS FOR: the map declares "two", the matrix does not
# run it. Every flow still belongs to a shard, so the union check is satisfied
# and only this guard refuses.
expect_workflow refuse shard-not-run 'jobs:
  smoke-job:
    strategy:
      matrix:
        shard: [one]
    steps:
      - run: run.sh --shard ${{ matrix.shard }}
'

# The mirror: a matrix naming a shard the map does not declare would fail at
# selection time in CI, but saying so here names the real file.
expect_workflow refuse extra-shard 'jobs:
  smoke-job:
    strategy:
      matrix:
        shard: [one, two, three]
    steps:
      - run: run.sh --shard ${{ matrix.shard }}
'

# Order must not matter; the comparison is between sets.
expect_workflow pass reordered 'jobs:
  smoke-job:
    strategy:
      matrix:
        shard: [two, one]
    steps:
      - run: run.sh --shard ${{ matrix.shard }}
'

# A job that passes --shard but whose matrix this parser cannot read must FAIL
# rather than be assumed to agree: assuming is the silent pass the guard exists
# to refuse, and a matrix rewritten in block form is exactly how that would
# happen.
expect_workflow refuse unreadable-matrix 'jobs:
  smoke-job:
    strategy:
      matrix:
        shard:
          - one
          - two
    steps:
      - run: run.sh --shard ${{ matrix.shard }}
'

# An UNSHARDED job — one run over every flow — is complete coverage with no
# matrix to agree with. This is also the state of the tree before the job-split
# workflow change lands, so it must pass.
expect_workflow pass unsharded 'jobs:
  smoke-job:
    steps:
      - run: run.sh
'

# A workflow that does not contain the job at all has nothing to say about it.
expect_workflow pass job-absent 'jobs:
  unrelated:
    steps:
      - run: true
'

# A workflow file that is missing entirely cannot be checked, and must not read
# as agreement.
if smoke::require_workflow_shards smoke-job "$work/wf-green/does-not-exist.yml" \
    > "$work/wf-missing.out" 2>&1; then
  printf 'selftest FAIL: wf-missing — wanted refuse, got pass\n'
  failures=1
fi

# --- Harness coverage ---------------------------------------------------------
#
# The mirror of the flow union check, for what a shard installs rather than what
# it runs. Its failure mode is quieter than it looks: the flow fails deep inside
# with "command not found", which reads as a broken smoke rather than as a shard
# map that forgot a harness.
#
# expect_harness_coverage WANT(pass|refuse) CASE KNOWN...
expect_harness_coverage() {
  local want="$1" name="$2"
  shift 2
  local got=pass
  smoke::require_shard_harness_coverage "$@" \
    > "$work/harness-$name.out" 2>&1 || got=refuse
  if [[ "$got" != "$want" ]]; then
    printf 'selftest FAIL: harness-%s — wanted %s, got %s\n' "$name" "$want" "$got"
    sed 's/^/    /' "$work/harness-$name.out"
    failures=1
  fi
}

# The shard map loaded above claims claude, codex and opencode.
expect_harness_coverage pass green claude codex opencode
# An installable harness no shard claims would be installed by no job.
expect_harness_coverage refuse unclaimed claude codex opencode gemini
# A shard naming a harness nothing can install is a typo that would surface as a
# missing binary halfway through a flow.
expect_harness_coverage refuse unknown-harness claude opencode
# Declaring no installable harnesses at all must be an error, not a free pass.
expect_harness_coverage refuse no-known

if [[ "$failures" -ne 0 ]]; then
  echo "smoke evidence selftest FAILED; refusing to trust any smoke result"
  exit 1
fi
echo "smoke evidence selftest: ok (evidence checker + manifest drift guards + shard-map union, workflow-matrix and harness-coverage guards)"
