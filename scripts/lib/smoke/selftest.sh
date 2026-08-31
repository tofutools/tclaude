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

# The deb822 classifier must tolerate formatting whitespace while still
# refusing a source file that mixes Microsoft and another vendor. This is kept
# here rather than as a one-off command in CI so every smoke run exercises the
# exact parser used before apt-get update.
apt_source_classifier_test() {
  local name="$1" want="$2" content="$3"
  local source_file="$work/apt-source-$name.sources"
  local got=refuse
  printf '%b' "$content" > "$source_file"
  if smoke::apt_source_is_microsoft_only "$source_file"; then
    got=pass
  fi
  if [[ "$got" != "$want" ]]; then
    printf 'selftest FAIL: apt-source-%s — wanted %s, got %s\n' "$name" "$want" "$got"
    failures=1
  fi
}

apt_source_classifier_test microsoft-trailing-space pass \
  'Types: deb\nURIs: https://packages.microsoft.com/repos/azure-cli/ \nSuites: noble\nComponents: main\n'
apt_source_classifier_test microsoft-non-http pass \
  'Types: deb\nURIs: file:packages.microsoft.com/repos/azure-cli/\nSuites: noble\nComponents: main\n'
apt_source_classifier_test mixed refuse \
  'Types: deb\nURIs: https://packages.microsoft.com/repos/azure-cli/ https://archive.ubuntu.com/ubuntu/\nSuites: noble\nComponents: main\n'

# GitHub's generated mirror list currently prefers an intermittently
# unreachable Azure endpoint. Prove that the workaround removes exactly that
# runner-image entry while preserving the working fallbacks and lookalikes.
cat > "$work/apt-mirrors.input" <<'EOF'
http://azure.archive.ubuntu.com/ubuntu/	priority:1
https://archive.ubuntu.com/ubuntu/	priority:2
https://security.ubuntu.com/ubuntu/	priority:3
https://azure.archive.ubuntu.com/ubuntu/	priority:4
http://azure.archive.ubuntu.com/ubuntu/extra	priority:5
EOF
cat > "$work/apt-mirrors.want" <<'EOF'
https://archive.ubuntu.com/ubuntu/	priority:2
https://security.ubuntu.com/ubuntu/	priority:3
https://azure.archive.ubuntu.com/ubuntu/	priority:4
http://azure.archive.ubuntu.com/ubuntu/extra	priority:5
EOF
smoke::apt_mirror_list_without_unreachable_azure "$work/apt-mirrors.input" \
  > "$work/apt-mirrors.got"
if ! cmp -s "$work/apt-mirrors.want" "$work/apt-mirrors.got"; then
  echo 'selftest FAIL: apt mirror isolation did not remove only the exact GitHub Azure mirror'
  diff -u "$work/apt-mirrors.want" "$work/apt-mirrors.got" || true
  failures=1
fi

# The runner mirror can stop producing output indefinitely while apt's own
# retry machinery remains alive. Prove that every shared update goes through
# both the transport limits and an outer deadline, without invoking sudo or apt
# in this safe-to-run-anywhere self-test.
apt_update_argv="$work/apt-update.argv"
(
  sudo() { printf '%s\n' "$@" > "$apt_update_argv"; }
  smoke::run_bounded_apt_update
)
cat > "$work/apt-update.want" <<'EOF'
-n
timeout
--kill-after=10s
180s
apt-get
-o
Acquire::Retries=2
-o
Acquire::http::Timeout=15
-o
Acquire::https::Timeout=15
update
--quiet
EOF
if ! cmp -s "$work/apt-update.want" "$apt_update_argv"; then
  echo 'selftest FAIL: apt update is not bounded by the reviewed transport and outer timeouts'
  diff -u "$work/apt-update.want" "$apt_update_argv" || true
  failures=1
fi

# External prerequisite clones occasionally arrive truncated on hosted
# runners. Prove the retry helper forwards the reviewed clone arguments, uses
# a fresh destination for every attempt, promotes only the successful one, and
# stops after exactly three failures. Git and sleep are shadowed, so this is
# deterministic and performs no network access or real backoff.
git_clone_retry_success="$work/git-clone-retry-success"
(
  retry_calls=0
  sleep() { :; }
  git() {
    retry_calls=$((retry_calls + 1))
    printf '%s\n' "$@" > "$work/git-clone-retry-$retry_calls.argv"
    local attempt_dir="${*: -1}"
    mkdir -p "$attempt_dir"
    printf '%s\n' "$retry_calls" > "$attempt_dir/attempt"
    (( retry_calls >= 3 ))
  }
  smoke::git_clone_retry https://example.invalid/passt "$git_clone_retry_success" \
    --quiet --depth 1 --branch pinned
  [[ "$retry_calls" -eq 3 ]]
) || {
  echo 'selftest FAIL: git clone retry did not succeed on its third attempt'
  failures=1
}
for attempt in 1 2 3; do
  cat > "$work/git-clone-retry-$attempt.want" <<EOF
clone
--quiet
--depth
1
--branch
pinned
https://example.invalid/passt
$git_clone_retry_success.clone-attempt-$attempt
EOF
  if ! cmp -s "$work/git-clone-retry-$attempt.want" "$work/git-clone-retry-$attempt.argv"; then
    printf 'selftest FAIL: git clone retry attempt %s did not preserve its arguments/path\n' "$attempt"
    failures=1
  fi
done
if [[ "$(cat "$git_clone_retry_success/attempt" 2>/dev/null || true)" != 3 ||
      ! -d "$git_clone_retry_success.clone-attempt-1" ||
      ! -d "$git_clone_retry_success.clone-attempt-2" ||
      -e "$git_clone_retry_success.clone-attempt-3" ]]; then
  echo 'selftest FAIL: git clone retry did not isolate attempts and promote only the success'
  failures=1
fi

git_clone_retry_failure="$work/git-clone-retry-failure"
retry_failure_calls="$work/git-clone-retry-failure.calls"
: > "$retry_failure_calls"
if (
  sleep() { :; }
  git() { printf 'x' >> "$retry_failure_calls"; return 1; }
  smoke::git_clone_retry https://example.invalid/passt "$git_clone_retry_failure" --quiet
); then
  echo 'selftest FAIL: git clone retry accepted three failed attempts'
  failures=1
fi
if [[ "$(wc -c < "$retry_failure_calls" | tr -d ' ')" -ne 3 || -e "$git_clone_retry_failure" ]]; then
  echo 'selftest FAIL: git clone retry did not stop after three failures without promotion'
  failures=1
fi

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

# --- The per-flow harness declaration -----------------------------------------
#
# Each flow says which harnesses it launches, in its own file, and each shard's
# install set is DERIVED as the union of its flows' declarations. The declaration
# lives in the flow because that is the only place it cannot be left behind: when
# it lived in the shard map, moving a flow to the other job without moving its
# harness satisfied every union check and then failed deep inside the flow with a
# "cannot install"-class error — loud, but attributed to the flow instead of to
# the declaration.
#
# What can still be got wrong is the declaration itself, so that is what these
# cases pin: a flow that declares nothing, one that declares an empty set, one
# that says `none` and means it, and one that says `none` alongside a real name.
# Every refusal case asserts the SPECIFIC message: the whole point of this change
# is that a failure moved from one place to another, and a case that only checked
# "something failed" would pass in both worlds and prove nothing.
#
# expect_flow_harnesses WANT(pass|refuse) CASE MESSAGE_SUBSTRING FLOW_BODY...
#   FLOW_BODY entries are <flow-name>=<flow::harnesses body>, or <flow-name>=
#   with an empty body for a flow that declares no flow::harnesses at all.
expect_flow_harnesses() {
  local want="$1" name="$2" wanted_message="$3"
  shift 3
  local dir="$work/flowharness-$name"
  mkdir -p "$dir/flows"
  local entry flow body manifest=""
  for entry in "$@"; do
    flow="${entry%%=*}"
    body="${entry#*=}"
    manifest+="$flow Test${flow}"$'\n'
    if [[ -z "$body" ]]; then
      printf 'flow::run() { :; }\n' > "$dir/flows/$flow.sh"
    else
      printf 'flow::run() { :; }\nflow::harnesses() { %s; }\n' "$body" \
        > "$dir/flows/$flow.sh"
    fi
  done
  printf '%s' "$manifest" > "$dir/manifest.txt"
  local got=pass
  {
    smoke::load_manifest "$dir/manifest.txt" "$dir/flows" &&
      smoke::load_flow_harnesses
  } > "$dir/out" 2>&1 || got=refuse
  if [[ "$got" != "$want" ]]; then
    printf 'selftest FAIL: flowharness-%s — wanted %s, got %s\n' "$name" "$want" "$got"
    sed 's/^/    /' "$dir/out"
    failures=1
    return
  fi
  # A refusal has to refuse the RIGHT thing. Without this, a guard that failed
  # for an unrelated reason — an unreadable flow, a manifest slip — would satisfy
  # every case below.
  if [[ -n "$wanted_message" ]] && ! grep -qF "$wanted_message" "$dir/out"; then
    printf 'selftest FAIL: flowharness-%s did not say %q\n' "$name" "$wanted_message"
    sed 's/^/    /' "$dir/out"
    failures=1
  fi
}

# The green case, including the two shapes that must not be confused: a real set
# and an explicit empty one.
expect_flow_harnesses pass green '' \
  '10-alpha=echo claude codex' '20-beta=echo none'
if [[ "${SMOKE_FLOW_HARNESSES[10-alpha]:-}" != "claude codex " ]]; then
  printf 'selftest FAIL: flowharness-green recorded %q for 10-alpha, wanted both names\n' \
    "${SMOKE_FLOW_HARNESSES[10-alpha]:-}"
  failures=1
fi
# `none` records the EMPTY string, and it must still be recorded — "declared
# none" and "never loaded" are the two states the derivation must tell apart.
if [[ -z "${SMOKE_FLOW_HARNESSES[20-beta]+set}" ]]; then
  printf 'selftest FAIL: flowharness-green did not record 20-beta at all\n'
  failures=1
elif [[ -n "${SMOKE_FLOW_HARNESSES[20-beta]}" ]]; then
  printf 'selftest FAIL: flowharness-green recorded %q for 20-beta, wanted the empty set\n' \
    "${SMOKE_FLOW_HARNESSES[20-beta]}"
  failures=1
fi

# THE ONE THIS SECTION EXISTS FOR: a flow with no declaration must be refused at
# VALIDATION, naming the flow — not left to fail inside the flow at run time with
# a missing binary.
expect_flow_harnesses refuse undeclared \
  "flow '20-beta' declares no flow::harnesses" \
  '10-alpha=echo claude' '20-beta='

# Printing nothing is not a declaration of nothing: it is the shape a broken
# declaration takes, so `none` has to be said out loud.
expect_flow_harnesses refuse empty-output \
  "flow '10-alpha' declares an empty harness set" \
  '10-alpha=:'

# ...and `none` alongside a real name is a contradiction rather than a superset.
# The message must name the COMPANIONS, not the whole set: "alongside none codex"
# would list `none` as one of the things `none` conflicts with, which is false
# and tells the reader nothing about what to drop.
expect_flow_harnesses refuse none-plus-name \
  "flow '10-alpha' declares 'none' alongside codex opencode;" \
  '10-alpha=echo none codex opencode'
if grep -qF "alongside none" "$work/flowharness-none-plus-name/out"; then
  printf "selftest FAIL: flowharness-none-plus-name listed 'none' as its own companion\n"
  sed 's/^/    /' "$work/flowharness-none-plus-name/out"
  failures=1
fi
# The token order is preserved, so `none` appearing LAST must still report only
# the real names — a companion list built by trimming a prefix or suffix would
# pass the case above and fail this one.
expect_flow_harnesses refuse none-plus-name-trailing \
  "flow '10-alpha' declares 'none' alongside codex opencode;" \
  '10-alpha=echo codex opencode none'

# A declaration the reader cannot evaluate must fail rather than record nothing.
expect_flow_harnesses refuse failing-declaration \
  "flow '10-alpha' flow::harnesses failed with status 9" \
  '10-alpha=return 9'

# ...and the statuses that would collide with a reserved sentinel if the outcome
# were carried in the exit status rather than on the first line. A flow whose
# declaration returns 3 must be reported as a FAILING declaration, never as
# "declares no flow::harnesses" — that would be a lie about a declaration
# sitting right there in the file, which is the same misattribution this whole
# change is about, one level down.
expect_flow_harnesses refuse failing-declaration-status-3 \
  "flow '10-alpha' flow::harnesses failed with status 3" \
  '10-alpha=return 3'
expect_flow_harnesses refuse failing-declaration-status-4 \
  "flow '10-alpha' flow::harnesses failed with status 4" \
  '10-alpha=return 4'

# A flow whose TOP LEVEL exits non-zero, with a perfectly good declaration
# sitting right there in the file. It must NOT be reported as the declaration
# failing — that is the same lie one trigger over from the reserved-status
# collision above, and it is why the verdict is read before the status is
# attributed. `expect_flow_harnesses` cannot build this one: the failure has to
# be at top level, outside flow::harnesses.
expect_flow_top_level() {
  local name="$1" body="$2" wanted="$3"
  local dir="$work/flowtop-$name"
  mkdir -p "$dir/flows"
  printf '10-alpha TestAlpha\n' > "$dir/manifest.txt"
  printf 'flow::run() { :; }\n%s\nflow::harnesses() { echo claude; }\n' "$body" \
    > "$dir/flows/10-alpha.sh"
  if {
    smoke::load_manifest "$dir/manifest.txt" "$dir/flows" &&
      smoke::load_flow_harnesses
  } > "$dir/out" 2>&1; then
    printf 'selftest FAIL: flowtop-%s — wanted refuse, got pass\n' "$name"
    failures=1
    return
  fi
  if ! grep -qF "$wanted" "$dir/out"; then
    printf 'selftest FAIL: flowtop-%s did not say %q\n' "$name" "$wanted"
    sed 's/^/    /' "$dir/out"
    failures=1
  fi
  # The discriminating half: it must not be blamed on the declaration, which is
  # defined and was never even called.
  if grep -qF 'flow::harnesses failed' "$dir/out"; then
    printf 'selftest FAIL: flowtop-%s blamed the declaration for a top-level failure\n' "$name"
    sed 's/^/    /' "$dir/out"
    failures=1
  fi
}

expect_flow_top_level exit-nonzero 'exit 5' \
  "flow '10-alpha' exited with status 5 before its harness declaration could be read"

# The mirror: a top-level `exit 0` short-circuits the file before either marker
# is printed. Status is zero and nothing about the declaration is known, so the
# no-verdict branch must refuse rather than fall through to an empty set.
expect_flow_top_level exit-zero 'exit 0' \
  "flow '10-alpha' produced no harness-declaration verdict"

# A flow that cannot be sourced at all: refused, and bash's own diagnostic is
# left visible so the reader gets a line number rather than only "could not be
# sourced". `unexpected end of file` is bash's, not ours — its presence in the
# captured output is the assertion that stderr was not swallowed.
mkdir -p "$work/flowharness-unsourceable/flows"
printf '10-alpha TestAlpha\n' > "$work/flowharness-unsourceable/manifest.txt"
printf 'flow::run() { :; }\nflow::harnesses() {\n' \
  > "$work/flowharness-unsourceable/flows/10-alpha.sh"
if {
  smoke::load_manifest "$work/flowharness-unsourceable/manifest.txt" \
    "$work/flowharness-unsourceable/flows" &&
    smoke::load_flow_harnesses
} > "$work/flowharness-unsourceable/out" 2>&1; then
  printf 'selftest FAIL: flowharness-unsourceable — wanted refuse, got pass\n'
  failures=1
else
  if ! grep -qF "flow '10-alpha' could not be sourced" \
      "$work/flowharness-unsourceable/out"; then
    printf 'selftest FAIL: flowharness-unsourceable did not name the flow\n'
    sed 's/^/    /' "$work/flowharness-unsourceable/out"
    failures=1
  fi
  if ! grep -q 'unexpected end of file' "$work/flowharness-unsourceable/out"; then
    printf "selftest FAIL: flowharness-unsourceable swallowed bash's own diagnostic\n"
    sed 's/^/    /' "$work/flowharness-unsourceable/out"
    failures=1
  fi
fi

# Duplicates would install the same harness twice and read as two claims.
expect_flow_harnesses refuse duplicate \
  "declares harness 'codex' twice" \
  '10-alpha=echo codex codex'

# Names index associative arrays and are interpolated into a
# `harnesses::install_$name` call, so the same charset gate the shard map uses
# applies here.
expect_flow_harnesses refuse glob-in-harness \
  "which is not a plain name" \
  '10-alpha=echo "*"'

# Reading declarations before a manifest would record none of them and then
# derive an empty install set for every shard.
if ( SMOKE_FLOW_FILES=(); smoke::load_flow_harnesses ) \
    > "$work/flowharness-no-manifest.out" 2>&1; then
  printf 'selftest FAIL: flowharness-no-manifest — wanted refuse, got pass\n'
  failures=1
fi

# --- The shard-map guards -----------------------------------------------------
#
# Splitting a shard's flows across CI jobs adds a third way to prove nothing,
# which neither set of guards above can see: a flow that has manifest evidence,
# has a flow file, passes every drift check — and is assigned to no job, so it
# never runs anywhere while every job stays green. The union check refuses that,
# and this section is what proves the union check actually refuses it.
#
# expect_shards WANT(pass|refuse) CASE MANIFEST_CONTENT SHARDS_CONTENT FLOW_NAME...
#   Each FLOW_NAME may carry its harness declaration as <name>:<harness>...;
#   a bare name declares `none`.
expect_shards() {
  local want="$1" name="$2" manifest_content="$3" shards_content="$4"
  shift 4
  local dir="$work/shards-$name"
  mkdir -p "$dir/flows"
  printf '%s' "$manifest_content" > "$dir/manifest.txt"
  printf '%s' "$shards_content" > "$dir/shards.txt"
  local flow spec harnesses
  for spec in "$@"; do
    flow="${spec%%:*}"
    harnesses="${spec#*:}"
    [[ "$harnesses" == "$spec" ]] && harnesses=none
    printf 'flow::run() { :; }\nflow::harnesses() { echo %s; }\n' "$harnesses" \
      > "$dir/flows/$flow.sh"
  done
  local got=pass
  {
    smoke::load_manifest "$dir/manifest.txt" "$dir/flows" &&
      smoke::load_flow_harnesses &&
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
# 10-alpha declares claude, 20-beta declares codex, 30-gamma declares opencode —
# one harness each, so a derived set says unambiguously which flows produced it.
SHARD_FLOWS=(10-alpha:claude 20-beta:codex 30-gamma:opencode)

# The green case: every flow assigned, and a shard map that declares flows only.
expect_shards pass green "$SHARD_MANIFEST" '# a comment
one flows      10-alpha 20-beta

two flows      30-gamma' "${SHARD_FLOWS[@]}"

# ...and the parse is inspected, not just the exit status: a reader that
# accepted the line while recording one flow would satisfy the case above and
# then silently run half the shard.
if [[ "${SMOKE_SHARD_FLOWS[one]:-}" != "10-alpha 20-beta " ]]; then
  printf 'selftest FAIL: shards-green recorded flows %q, wanted both\n' \
    "${SMOKE_SHARD_FLOWS[one]:-}"
  failures=1
fi
# THE DERIVATION, inspected rather than inferred. Nothing in the shard map above
# mentions a harness; "claude codex " can only have come from the two flows'
# own declarations, unioned.
if [[ "${SMOKE_SHARD_HARNESSES[one]:-}" != "claude codex " ]]; then
  printf 'selftest FAIL: shards-green derived harnesses %q for shard one, wanted "claude codex "\n' \
    "${SMOKE_SHARD_HARNESSES[one]:-}"
  failures=1
fi
if [[ "${SMOKE_SHARD_HARNESSES[two]:-}" != "opencode " ]]; then
  printf 'selftest FAIL: shards-green derived harnesses %q for shard two, wanted "opencode "\n' \
    "${SMOKE_SHARD_HARNESSES[two]:-}"
  failures=1
fi

# THE FALSIFICATION THIS CHANGE EXISTS FOR, run as a positive: 20-beta MOVES from
# shard one to shard two, and NOTHING else changes — no harness list is edited,
# because there is none to edit. Its codex declaration must move with it. Under
# the old per-shard scheme this exact edit left codex behind in shard one and
# failed inside the flow at run time with a "cannot install"-class error.
expect_shards pass moved-flow "$SHARD_MANIFEST" 'one flows      10-alpha

two flows      20-beta 30-gamma' "${SHARD_FLOWS[@]}"
if [[ "${SMOKE_SHARD_HARNESSES[one]:-}" != "claude " ]]; then
  printf 'selftest FAIL: shards-moved-flow left shard one with %q, wanted "claude "\n' \
    "${SMOKE_SHARD_HARNESSES[one]:-}"
  failures=1
fi
if [[ "${SMOKE_SHARD_HARNESSES[two]:-}" != "codex opencode " ]]; then
  printf 'selftest FAIL: shards-moved-flow derived %q for shard two, wanted "codex opencode "\n' \
    "${SMOKE_SHARD_HARNESSES[two]:-}"
  failures=1
fi

# A shard whose flows all declare `none` installs nothing, and that is a correct
# derivation rather than a forgotten line — the guard that used to refuse it was
# guarding a declaration that no longer lives here.
expect_shards pass derived-empty '10-alpha TestAlpha
' 'one flows 10-alpha
' 10-alpha
if [[ -z "${SMOKE_SHARD_HARNESSES[one]+set}" || -n "${SMOKE_SHARD_HARNESSES[one]}" ]]; then
  printf 'selftest FAIL: shards-derived-empty derived %q, wanted the empty set\n' \
    "${SMOKE_SHARD_HARNESSES[one]:-<unset>}"
  failures=1
fi
# ...carried all the way through SELECTION, not just derivation. The empty set is
# a path the entrypoint takes at run time — `read -a` on an empty here-string,
# then a `for` over a zero-length array under `set -u` — and a case that stopped
# at load_shards would leave the one shape claimed-supported and never executed.
#
# PRE-POISONED on purpose. This is the first selection the file performs, so
# SMOKE_SELECTED_HARNESSES is still at its `declare -a … =()` initial value and
# an empty result would be indistinguishable from nothing having happened —
# the case would pass in a world where selection did not clear at all. Seeding a
# stale selection first is what makes the emptiness an observation.
SMOKE_SELECTED_HARNESSES=(stale-claude stale-codex)
if smoke::select_shard one > "$work/select-derived-empty.out" 2>&1; then
  if [[ ${#SMOKE_SELECTED_HARNESSES[@]} -ne 0 ]]; then
    printf 'selftest FAIL: select-derived-empty selected %q, wanted nothing\n' \
      "${SMOKE_SELECTED_HARNESSES[*]}"
    failures=1
  fi
  # The SHAPE of the loop run.sh performs, under the same `set -euo pipefail`:
  # zero iterations, no unbound-variable abort. Stated honestly — this is a
  # re-implementation, not run.sh's own line, so it cannot catch run.sh's length
  # guard being deleted. What it does pin is that an empty selection is safe to
  # iterate at all, which is the property the guard exists to preserve.
  if ! (
    set -euo pipefail
    if [[ ${#SMOKE_SELECTED_HARNESSES[@]} -gt 0 ]]; then
      for h in "${SMOKE_SELECTED_HARNESSES[@]}"; do echo "$h"; done
    fi
  ) > /dev/null 2>&1; then
    printf 'selftest FAIL: select-derived-empty cannot be iterated under set -u\n'
    failures=1
  fi
else
  printf 'selftest FAIL: select-derived-empty — wanted pass, got refuse\n'
  sed 's/^/    /' "$work/select-derived-empty.out"
  failures=1
fi

# THE ONE THIS SECTION EXISTS FOR: 30-gamma has evidence and a flow file, but no
# shard runs it. Every other guard in this file passes; only this one refuses.
expect_shards refuse union-gap "$SHARD_MANIFEST" 'one flows      10-alpha 20-beta
' "${SHARD_FLOWS[@]}"

# A shard naming a flow the manifest does not: evidence assigned to a job for a
# smoke that does not exist.
expect_shards refuse unknown-flow "$SHARD_MANIFEST" 'one flows      10-alpha 20-beta 30-gamma 40-delta
' "${SHARD_FLOWS[@]}"

# NO "shard with no flows" and no "shard with no harnesses" case here, and both
# absences are deliberate. A shard now enters the map only through its `flows`
# line, so "declared but with no flows" is no longer constructible — the check
# for it survives in driver.sh as an invariant, the way the selection count
# check does, but it has nothing to assert against. And the install set is
# derived, so the declaration that can be forgotten is the FLOW's:
# flowharness-undeclared above is where that guard is proven.

# Structural drift: a repeated key silently discards one of the two lists, an
# unknown key is a typo that would drop the line, and a repeated flow inside one
# shard would run the same smoke twice while reading as coverage of two.
expect_shards refuse duplicate-key "$SHARD_MANIFEST" 'one flows      10-alpha 20-beta
one flows      30-gamma
' "${SHARD_FLOWS[@]}"
expect_shards refuse unknown-key "$SHARD_MANIFEST" 'one flowz      10-alpha 20-beta 30-gamma
' "${SHARD_FLOWS[@]}"
expect_shards refuse duplicate-flow "$SHARD_MANIFEST" 'one flows      10-alpha 10-alpha 20-beta 30-gamma
' "${SHARD_FLOWS[@]}"

# A shard map still carrying the OLD per-shard `harnesses` key must be refused by
# name, not ignored. Silently skipping it is the worst of both worlds: the map
# would read as declaring an install set while the derivation quietly overrode it.
expect_shards refuse stale-harness-key "$SHARD_MANIFEST" 'one flows      10-alpha 20-beta 30-gamma
one harnesses  claude codex opencode
' "${SHARD_FLOWS[@]}"
if ! grep -qF "harnesses are declared per flow" "$work/shards-stale-harness-key/out"; then
  printf 'selftest FAIL: shards-stale-harness-key did not say where harnesses are declared\n'
  sed 's/^/    /' "$work/shards-stale-harness-key/out"
  failures=1
fi

# Names index associative arrays and are compared through unquoted expansions,
# so a glob or `@` in one would alias every key or expand against the repo root.
expect_shards refuse glob-in-flow "$SHARD_MANIFEST" 'one flows      * 10-alpha 20-beta 30-gamma
' "${SHARD_FLOWS[@]}"
expect_shards refuse glob-in-shard-name "$SHARD_MANIFEST" '@ flows      10-alpha 20-beta 30-gamma
' "${SHARD_FLOWS[@]}"

# A shard map that declares nothing assigns nothing, and a missing one is not a
# permissive default.
expect_shards refuse empty-map "$SHARD_MANIFEST" '# only a comment
' "${SHARD_FLOWS[@]}"
expect_shards refuse comment-only-list "$SHARD_MANIFEST" 'one flows      # nothing here
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

# ...and deriving install sets from flow declarations that were never read would
# hand every shard an empty one — every job installing nothing, every flow that
# needs a harness failing with "not found".
#
# The MESSAGE is asserted, not just the refusal. A bare exit-status check does
# not discriminate here: the per-flow `${...+set}` check inside the derivation
# refuses the same input for a different reason, so this case would stay green
# with the ordering guard deleted and would prove nothing about the guard its
# name claims.
if ( SMOKE_FLOW_HARNESSES=(); smoke::load_shards "$work/shards-green/shards.txt" ) \
    > "$work/shards-no-flow-harnesses.out" 2>&1; then
  printf 'selftest FAIL: shards-no-flow-harnesses — wanted refuse, got pass\n'
  failures=1
elif ! grep -qF "called before smoke::load_flow_harnesses" \
    "$work/shards-no-flow-harnesses.out"; then
  printf 'selftest FAIL: shards-no-flow-harnesses refused for the wrong reason\n'
  sed 's/^/    /' "$work/shards-no-flow-harnesses.out"
  failures=1
fi

# --- Shard selection ----------------------------------------------------------
#
# Selection has its own way to prove nothing: a name that matches no shard must
# fail rather than resolve to an empty flow list, and a name that does match must
# narrow the run to exactly that shard's flows and derived harnesses.
smoke::load_manifest "$work/shards-green/manifest.txt" "$work/shards-green/flows" \
  > /dev/null 2>&1
smoke::load_flow_harnesses > /dev/null 2>&1
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
# with "command not found", which reads as a broken smoke rather than as a
# declaration that forgot a harness.
#
# The check reads the DERIVED shard sets, so what it proves is unchanged; only
# where the sets come from moved. The mirror direction names the declaring FLOW,
# because the flow file is where the typo is.
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

# The flows loaded above declare claude, codex and opencode; the shard sets
# derived from them claim exactly those three.
expect_harness_coverage pass green claude codex opencode
# An installable harness no flow declares would be installed by no job.
expect_harness_coverage refuse unclaimed claude codex opencode gemini
if ! grep -qF "harness 'gemini' is installable but no flow declares it" \
    "$work/harness-unclaimed.out"; then
  printf 'selftest FAIL: harness-unclaimed did not name the unclaimed harness\n'
  sed 's/^/    /' "$work/harness-unclaimed.out"
  failures=1
fi
# A flow declaring a harness nothing can install is a typo that would surface as
# a missing binary halfway through a flow. The message must name 20-beta, which
# is the file the fix belongs in — a message naming only the shard would send
# the reader to a file that no longer declares harnesses at all.
expect_harness_coverage refuse unknown-harness claude opencode
if ! grep -qF "flow(s) 20-beta declare harness 'codex', which this entrypoint cannot install" \
    "$work/harness-unknown-harness.out"; then
  printf 'selftest FAIL: harness-unknown-harness did not name the declaring flow\n'
  sed 's/^/    /' "$work/harness-unknown-harness.out"
  failures=1
fi
# Declaring no installable harnesses at all must be an error, not a free pass.
expect_harness_coverage refuse no-known

# One defect, reported ONCE. Two shards legitimately claim the same harness when
# a flow in each declares it, and iterating the raw per-shard concatenation would
# print the same "cannot install" line once per claiming shard — a single typo
# rendered as two findings, which is the misattribution this change is about
# wearing a different hat.
mkdir -p "$work/harness-dup/flows"
printf '10-alpha TestAlpha\n20-beta TestBeta\n' > "$work/harness-dup/manifest.txt"
printf 'one flows 10-alpha\ntwo flows 20-beta\n' > "$work/harness-dup/shards.txt"
printf 'flow::run() { :; }\nflow::harnesses() { echo codex; }\n' \
  > "$work/harness-dup/flows/10-alpha.sh"
printf 'flow::run() { :; }\nflow::harnesses() { echo codex; }\n' \
  > "$work/harness-dup/flows/20-beta.sh"
if {
  smoke::load_manifest "$work/harness-dup/manifest.txt" "$work/harness-dup/flows" &&
    smoke::load_flow_harnesses &&
    smoke::load_shards "$work/harness-dup/shards.txt"
} > "$work/harness-dup/load.out" 2>&1; then
  smoke::require_shard_harness_coverage claude > "$work/harness-dup/out" 2>&1 || true
  # Counted on the plain ERROR: line only. smoke::error also emits a `::error::`
  # workflow annotation for the same message, so counting every occurrence would
  # report 2 for a single finding and this case would fail on the correct code.
  dup_lines="$(grep -c "^ERROR:.*cannot install" "$work/harness-dup/out" || true)"
  if [[ "${dup_lines:-0}" -ne 1 ]]; then
    printf 'selftest FAIL: harness-dup reported the same uninstallable harness %s times, wanted 1\n' \
      "${dup_lines:-0}"
    sed 's/^/    /' "$work/harness-dup/out"
    failures=1
  fi
  # ...and both declaring flows are still named, so deduplicating the harness
  # must not cost the attribution the message exists for.
  if ! grep -qF "flow(s) 10-alpha 20-beta declare harness 'codex'" \
      "$work/harness-dup/out"; then
    printf 'selftest FAIL: harness-dup did not name both declaring flows\n'
    sed 's/^/    /' "$work/harness-dup/out"
    failures=1
  fi
else
  printf 'selftest FAIL: harness-dup fixture did not load\n'
  sed 's/^/    /' "$work/harness-dup/load.out"
  failures=1
fi

# --- fixture teardown hygiene (TCL-881) -------------------------------------
#
# These two helpers exist because a leaked fixture does not fail loudly: it
# fails the NEXT flow, on a port or a hostname that is still answering from the
# previous one, and that reads like a policy result. Proving them here costs
# nothing and needs no privileges — `sudo` is shadowed by a function, which is
# exactly the seam the real helper goes through.

# smoke::kill_listener must reach the process the WRAPPER is holding, not only
# the wrapper: that is the whole defect it was written for.
sudo() { "$@"; }
wrapper_log="$work/wrapper.log"
( sleep 30 & echo "$!" > "$work/child.pid"; wait ) >"$wrapper_log" 2>&1 &
wrapper_pid=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
  [[ -s "$work/child.pid" ]] && break
  sleep 0.2
done
child_pid="$(cat "$work/child.pid" 2>/dev/null || true)"
if [[ -z "$child_pid" ]]; then
  echo "selftest FAIL: kill_listener case could not start its wrapped child"
  failures=1
else
  smoke::kill_listener "$wrapper_pid"
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    kill -0 "$child_pid" 2>/dev/null || break
    sleep 0.2
  done
  if kill -0 "$child_pid" 2>/dev/null; then
    printf 'selftest FAIL: kill_listener left the wrapped process %s alive\n' "$child_pid"
    kill -KILL "$child_pid" 2>/dev/null || true
    failures=1
  fi
fi
kill -KILL "$wrapper_pid" 2>/dev/null || true
unset -f sudo

# The call shape every cleanup uses: an EMPTY or unset array under `set -u`.
# `"${pids[@]:-}"` expands to one empty argument there, and a helper that did
# not skip it — or that returned non-zero — would abort the very teardown it
# was called from.
(
  set -euo pipefail
  # shellcheck source=common.sh
  source "$here/common.sh"
  empty=()
  smoke::kill_listener "${empty[@]:-}"
  smoke::kill_listener ""
) || {
  echo 'selftest FAIL: kill_listener does not tolerate an empty pid list'
  failures=1
}

# smoke::trap_cleanup's INT/TERM half, proven the way it actually has to be
# proven. The obvious test does NOT work: bash runs an EXIT trap even when a
# fatal signal kills the shell, so "did the cleanup run" cannot tell the helper
# apart from a bare `trap ... EXIT`. Status discriminates for INT — an
# untrapped SIGINT exits 0 — but NOT for TERM, where 143 is what death by
# signal reports anyway, so the TERM case rests on the installed-handler
# assertion beside it.
#
# What the helper actually buys is the FAILURE: a script interrupted with
# SIGINT and no INT trap exits 0, and smoke::run_flows judges a flow by
# PIPESTATUS[0]. So the case below sends the signal FROM OUTSIDE to a child
# shell blocked in sleep — the real shape — and pins the status. It also asks
# the shell what traps it installed, which is the direct statement and cannot
# be satisfied by bash's own signal handling.
expect_trap_cleanup_signal() {
  local case="$1" signal="$2" want_status="$3"
  local marks="$work/$case.marks" ready="$work/$case.ready"
  local script="$work/$case.sh" traps="$work/$case.traps"
  : > "$marks"
  rm -f "$ready"
  cat > "$script" <<SCRIPT
set -euo pipefail
source "$here/common.sh"
mark() { echo x >> "$marks"; }
smoke::trap_cleanup mark
trap -p INT TERM > "$traps"
: > "$ready"
sleep 10
SCRIPT
  # Job control ON for the launch: bash sets SIGINT to SIG_IGN for background
  # jobs of a non-interactive shell, and a trap cannot override an INHERITED
  # ignore — so without `set -m` the child never sees the signal and this case
  # would report the failure it is meant to detect, for the wrong reason.
  set -m
  bash "$script" &
  local child=$!
  set +m
  local waited=0
  while [[ ! -f "$ready" && "$waited" -lt 50 ]]; do
    sleep 0.1
    waited=$((waited + 1))
  done
  kill -"$signal" "$child" 2>/dev/null || true
  local status=0
  wait "$child" && status=0 || status=$?
  # `wait` has already reaped the child, but its foreground `sleep` is orphaned
  # when the handler exits out from under it. Kill the process group `set -m`
  # gave the child — leaving a stray sleep behind in a teardown-hygiene test
  # would be a poor joke.
  kill -KILL -- "-$child" 2>/dev/null || true

  if [[ "$status" -ne "$want_status" ]]; then
    printf 'selftest FAIL: trap_cleanup/%s exited %s, wanted %s\n' \
      "$case" "$status" "$want_status"
    failures=1
  fi
  local ran
  ran="$(wc -l < "$marks" | tr -d ' ')"
  if [[ "$ran" -ne 1 ]]; then
    printf 'selftest FAIL: trap_cleanup/%s ran cleanup %s times, wanted exactly 1\n' \
      "$case" "$ran"
    failures=1
  fi
  if ! grep -q "SIG$signal" "$traps"; then
    printf 'selftest FAIL: trap_cleanup/%s installed no SIG%s handler\n' \
      "$case" "$signal"
    failures=1
  fi
}

# INT is the discriminating one — and the one GitHub sends first on a cancelled
# job. Without the helper's handler this child exits 0, which smoke::run_flows
# would read as a passing flow.
expect_trap_cleanup_signal int INT 130
# TERM cannot discriminate on status alone (a shell killed by TERM reports 143
# too), so this case rests on the installed-handler assertion beside it.
expect_trap_cleanup_signal term TERM 143

# The plain-exit case: the cleanup runs, ONCE, and the script's own status is
# passed through rather than replaced by the trap.
expect_trap_cleanup_exit() {
  local marks="$work/exit.marks"
  : > "$marks"
  local status=0
  (
    # shellcheck source=common.sh
    source "$here/common.sh"
    mark() { echo x >> "$marks"; }
    smoke::trap_cleanup mark
    exit 7
  ) >/dev/null 2>&1 || status=$?
  local ran
  ran="$(wc -l < "$marks" | tr -d ' ')"
  if [[ "$status" -ne 7 ]]; then
    printf 'selftest FAIL: trap_cleanup/exit exited %s, wanted 7\n' "$status"
    failures=1
  fi
  if [[ "$ran" -ne 1 ]]; then
    printf 'selftest FAIL: trap_cleanup/exit ran cleanup %s times, wanted exactly 1\n' "$ran"
    failures=1
  fi
}

expect_trap_cleanup_exit

if [[ "$failures" -ne 0 ]]; then
  echo "smoke evidence selftest FAILED; refusing to trust any smoke result"
  exit 1
fi
echo "smoke evidence selftest: ok (apt-source guards + clone retry + evidence checker + manifest drift guards + per-flow harness declarations + shard-map union, derived-install-set, workflow-matrix and harness-coverage guards + fixture teardown)"
