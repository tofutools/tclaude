#!/usr/bin/env bash
# Entrypoint for the filtered-network proxy smokes.
#
# CI invokes exactly this and nothing else. Everything that decides what runs,
# what counts as evidence, which harness versions are pinned and how fixtures
# are built lives in this directory — so extending the smokes is a repo change
# an agent can make and a reviewer can read, never a workflow merge.
#
# NEVER RUN THIS LOCALLY. The flows build real bubblewrap sandboxes, take over
# /etc/hosts, and create network namespaces with sudo. `selftest.sh` is the part
# that is safe to run anywhere, and it is what proves the evidence discipline.
#
# --validate-only runs the self-test and every manifest/flow consistency check
# and then stops, touching no sandbox and no network. It is the part of this
# entrypoint a developer or a pre-commit check can safely run, and it is how the
# drift guards below are proven without executing a smoke.
set -euo pipefail

validate_only=0
case "${1:-}" in
  --validate-only) validate_only=1 ;;
  "")              validate_only=0 ;;
  *)
    printf 'usage: %s [--validate-only]\n' "${BASH_SOURCE[0]}" >&2
    printf 'unrecognized argument: %s\n' "$1" >&2
    # Refusing an unknown argument matters more here than usually: the
    # destructive path is the DEFAULT, so a typo like --validate or --dry-run
    # would otherwise build sandboxes, npm-install harnesses and rewrite the
    # caller's /etc/hosts while they believed they had asked for a dry run.
    exit 2
    ;;
esac

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
cd "$repo_root"

# shellcheck source=lib/common.sh
source "$here/lib/common.sh"
# shellcheck source=lib/evidence.sh
source "$here/lib/evidence.sh"
# shellcheck source=lib/fixture.sh
source "$here/lib/fixture.sh"
# shellcheck source=lib/harnesses.sh
source "$here/lib/harnesses.sh"
# shellcheck source=lib/prereqs.sh
source "$here/lib/prereqs.sh"

export SMOKE_ARTIFACTS="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/filtered-proxy-smoke"
export SMOKE_HOSTS_BACKUP="$SMOKE_ARTIFACTS/etc-hosts.before"
export SMOKE_TCLAUDE_BINARY="$SMOKE_ARTIFACTS/tclaude"
mkdir -p "$SMOKE_ARTIFACTS"

# 1. Prove the evidence checker before trusting any result it produces. A guard
#    proven once at review time can rot; one that proves itself on every run
#    cannot. This needs no sandbox, so it is also the falsifiability check a
#    developer can run locally.
smoke::log "Self-testing the evidence checker"
bash "$here/selftest.sh"

# 2. Read and validate the manifest. An unparseable or empty manifest, or a
#    flow that claims no tests, must stop the run: those are exactly the states
#    in which everything downstream would pass vacuously.
declare -A REQUIRED_BY_FLOW=()
manifest="$here/manifest.txt"
# The `|| [[ -n ... ]]` tail matters: read returns non-zero at EOF, so a final
# line with no trailing newline would otherwise be skipped — silently dropping a
# required test while every drift guard still passed.
while read -r flow test_name _rest || [[ -n "${flow:-}" ]]; do
  [[ -z "${flow:-}" || "$flow" == \#* ]] && continue
  if [[ -z "${test_name:-}" ]]; then
    smoke::error "manifest line for flow '$flow' names no test"
    exit 1
  fi
  REQUIRED_BY_FLOW["$flow"]+="$test_name "
done < "$manifest"

mapfile -t flow_files < <(find "$here/flows" -maxdepth 1 -name '*.sh' | sort)
if [[ ${#flow_files[@]} -eq 0 ]]; then
  smoke::error "no flows found under $here/flows"
  exit 1
fi

# Every flow must be represented in the manifest, and every manifest entry must
# correspond to a flow. Either kind of drift means a smoke is running without
# recorded evidence, or evidence is claimed for a smoke that no longer runs.
declare -A SEEN_FLOW=()
for file in "${flow_files[@]}"; do
  name="$(basename "$file" .sh)"
  SEEN_FLOW["$name"]=1
  if [[ -z "${REQUIRED_BY_FLOW[$name]:-}" ]]; then
    smoke::error "flow '$name' declares no required tests in manifest.txt; it could not fail"
    exit 1
  fi
done
for name in "${!REQUIRED_BY_FLOW[@]}"; do
  if [[ -z "${SEEN_FLOW[$name]:-}" ]]; then
    smoke::error "manifest names flow '$name', but flows/$name.sh does not exist"
    exit 1
  fi
done

if [[ "$validate_only" -eq 1 ]]; then
  echo "filtered-proxy smoke: manifest and flows validate cleanly"
  exit 0
fi

# Past this point the script is destructive: it builds real sandboxes, installs
# packages and harnesses, and rewrites /etc/hosts. When the logic lived in a
# workflow it could not be run by accident; now that it is an ordinary file in
# the repo, that protection has to be written down.
if [[ "${CI:-}" != "true" && "${TCLAUDE_ALLOW_LOCAL_PROXY_SMOKE:-}" != "1" ]]; then
  smoke::error "refusing to run the real smokes outside CI"
  cat >&2 <<'TXT'
This builds real bubblewrap sandboxes, installs packages and harnesses, and
temporarily rewrites /etc/hosts. Run one of these instead:

  scripts/filtered-proxy-smoke/run.sh --validate-only   # manifest + flow checks
  scripts/filtered-proxy-smoke/selftest.sh              # evidence checker

Set TCLAUDE_ALLOW_LOCAL_PROXY_SMOKE=1 only if you genuinely mean it.
TXT
  exit 2
fi

# Whatever happens from here, the runner's resolver goes back. Flows restore it
# too; this is the backstop for a cancelled job, which fires no flow trap.
trap fixture::hosts_restore EXIT INT TERM

# 3. Prerequisites. Installed first, then asserted individually, so a missing
#    tool is named rather than surfacing later as a boundary that appeared to
#    refuse something.
prereqs::install
smoke::require_command go sudo ip socat curl npm node bwrap git || exit 1

smoke::log "Building tclaude for the smoke launches"
go build -o "$SMOKE_TCLAUDE_BINARY" .

harnesses::unlock_userns
harnesses::install_codex
harnesses::install_claude

# 4. Run each flow in a subshell so a flow's trap, cwd and variables cannot
#    leak into the next one, then check its evidence.
overall=0
declare -A FLOW_STATUS=()
for file in "${flow_files[@]}"; do
  name="$(basename "$file" .sh)"
  log="$SMOKE_ARTIFACTS/$name.log"
  smoke::log "Running flow: $name"
  status=0
  (
    set -euo pipefail
    # shellcheck source=/dev/null
    source "$file"
    flow::run
  ) 2>&1 | tee "$log" || true
  # Both halves are inspected: PIPESTATUS[0] is the flow, [1] is tee. Reading
  # only the flow would let a failed tee — which means the log this run's
  # evidence is read from is incomplete — resolve to success.
  status="${PIPESTATUS[0]}"
  if [[ "${PIPESTATUS[1]:-0}" -ne 0 ]]; then
    smoke::error "could not write the flow log $log; its evidence is unreadable"
    status=1
  fi

  # The exit status and the evidence are checked SEPARATELY and both must hold.
  # `go test` exits 0 for a skip, and a flow can also die before reaching its
  # tests at all; neither is evidence.
  read -r -a required <<< "${REQUIRED_BY_FLOW[$name]}"
  evidence_output=""
  if ! evidence_output="$(require_passed_tests "$log" "${required[@]}" 2>&1)"; then
    status=1
  fi

  if [[ "$status" -ne 0 ]]; then
    overall=1
    FLOW_STATUS["$name"]="FAILED"
    {
      printf '### Filtered-proxy smoke flow `%s` did not complete\n\n' "$name"
      printf 'A skip, missing/renamed test, build-tag mismatch, or zero-test success is a hard failure.\n\n'
      if [[ -n "$evidence_output" ]]; then
        printf '```text\n%s\n```\n\n' "$evidence_output"
      fi
      if ( source "$file"; declare -F flow::describe >/dev/null ); then
        printf 'What this flow must show:\n\n```text\n'
        ( source "$file"; flow::describe )
        printf '```\n'
      fi
    } | smoke::summary
    smoke::error "filtered-proxy smoke flow '$name' did not report the evidence it must"
  else
    FLOW_STATUS["$name"]="passed"
    if ( source "$file"; declare -F flow::report >/dev/null ); then
      {
        printf '### Filtered-proxy smoke `%s`\n\n```text\n' "$name"
        ( source "$file"; flow::report "$log" )
        printf '```\n'
      } | smoke::summary
    fi
  fi
done

{
  printf '### Filtered-proxy smoke evidence\n\n'
  printf '| flow | required tests | result |\n| -- | -- | -- |\n'
  for file in "${flow_files[@]}"; do
    name="$(basename "$file" .sh)"
    printf '| `%s` | `%s` | %s |\n' \
      "$name" "${REQUIRED_BY_FLOW[$name]% }" "${FLOW_STATUS[$name]:-not run}"
  done
} | smoke::summary

exit "$overall"
