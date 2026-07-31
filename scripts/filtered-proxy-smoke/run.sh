#!/usr/bin/env bash
# Entrypoint for the filtered-network proxy smokes.
#
# CI invokes exactly this and nothing else. Everything that decides what runs,
# what counts as evidence, which harness versions are pinned and how fixtures
# are built lives in this directory and in the shared smoke lib — so extending
# the smokes is a repo change an agent can make and a reviewer can read, never a
# workflow merge.
#
# The manifest discipline, the evidence checker and the fixture helpers are
# SHARED with the other smoke shards under scripts/lib/smoke/. What stays here
# is what is specific to these smokes: the host packages they need, the harness
# pins, and the flows themselves.
#
# NEVER RUN THIS LOCALLY. The flows build real bubblewrap sandboxes, take over
# /etc/hosts, and create network namespaces with sudo. `selftest.sh` is the part
# that is safe to run anywhere, and it is what proves the evidence discipline.
#
# --validate-only runs the self-test and every manifest/flow/shard consistency
# check and then stops, touching no sandbox and no network. It is the part of
# this entrypoint a developer or a pre-commit check can safely run, and it is how
# the drift guards below are proven without executing a smoke.
#
# --shard NAME (or SMOKE_SHARD=NAME) runs only the flows shards.txt assigns to
# that shard, and installs only the harnesses that shard needs. With no shard the
# entrypoint runs everything, exactly as it did before the split existed — so a
# developer, a validate-only check and a future single-job CI all keep working
# without knowing shards exist. The shard map is loaded and its union coverage
# asserted EITHER WAY: a hole in it must fail the unsharded run too, or the guard
# would only exist in the configuration that already skips flows.
set -euo pipefail

validate_only=0
shard=""
shard_set=0
# SMOKE_SHARD is held to the SAME rule as --shard, including when it is set to
# the empty string: exported-but-empty is the shape a CI expression that resolved
# to nothing produces, and treating it as "run everything" would silently make
# one job do another shard's work while looking like it had been given a shard.
if [[ -n "${SMOKE_SHARD+set}" ]]; then
  if [[ -z "$SMOKE_SHARD" ]]; then
    printf '%s\n' 'SMOKE_SHARD is set but empty; unset it to run every shard' >&2
    exit 2
  fi
  shard="$SMOKE_SHARD"
  shard_set=1
fi

usage() {
  printf 'usage: %s [--validate-only] [--shard NAME]\n' "${BASH_SOURCE[0]}" >&2
}

# An explicit --shard overrides SMOKE_SHARD, which is the ordinary precedence and
# is what makes a one-off local invocation possible in a shell that exports one.
# A REPEATED --shard is refused instead: there is no sensible precedence between
# two of them, and silently keeping the last would run one shard while the
# command line says two — the same ambiguity every other argument here refuses.
shard_from_flag=0
require_one_shard() {
  if [[ "$shard_from_flag" -eq 1 ]]; then
    usage
    printf '%s\n' '--shard given more than once' >&2
    exit 2
  fi
  shard_from_flag=1
}

# Refusing an unknown argument matters more here than usually: the destructive
# path is the DEFAULT, so a typo like --validate or --dry-run would otherwise
# build sandboxes, npm-install harnesses and rewrite the caller's /etc/hosts
# while they believed they had asked for a dry run. A --shard with no value is
# refused for the mirror-image reason: silently falling back to "run everything"
# would turn a mistyped CI invocation into a job that quietly did another shard's
# work as well.
while [[ $# -gt 0 ]]; do
  case "$1" in
    --validate-only) validate_only=1; shift ;;
    --shard)
      if [[ $# -lt 2 || -z "$2" ]]; then
        usage
        printf '%s\n' '--shard requires a shard name' >&2
        exit 2
      fi
      require_one_shard
      shard="$2"; shard_set=1; shift 2 ;;
    --shard=*)
      shard="${1#--shard=}"
      if [[ -z "$shard" ]]; then
        usage
        printf '%s\n' '--shard requires a shard name' >&2
        exit 2
      fi
      require_one_shard
      shard_set=1; shift ;;
    *)
      usage
      printf 'unrecognized argument: %s\n' "$1" >&2
      exit 2
      ;;
  esac
done

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
shared="$repo_root/scripts/lib/smoke"
cd "$repo_root"

# shellcheck source=../lib/smoke/common.sh
source "$shared/common.sh"
# shellcheck source=../lib/smoke/evidence.sh
source "$shared/evidence.sh"
# shellcheck source=../lib/smoke/driver.sh
source "$shared/driver.sh"
# shellcheck source=../lib/smoke/fixture.sh
source "$shared/fixture.sh"
# shellcheck source=../lib/smoke/sandbox.sh
source "$shared/sandbox.sh"
# shellcheck source=lib/harnesses.sh
source "$here/lib/harnesses.sh"
# shellcheck source=lib/prereqs.sh
source "$here/lib/prereqs.sh"

export SMOKE_ARTIFACTS="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/filtered-proxy-smoke"
export SMOKE_HOSTS_BACKUP="$SMOKE_ARTIFACTS/etc-hosts.before"
export SMOKE_TCLAUDE_BINARY="$SMOKE_ARTIFACTS/tclaude"
mkdir -p "$SMOKE_ARTIFACTS"

# 1. Prove the evidence checker and the manifest guards before trusting any
#    result they produce. A guard proven once at review time can rot; one that
#    proves itself on every run cannot. This needs no sandbox, so it is also the
#    falsifiability check a developer can run locally.
smoke::log "Self-testing the shared evidence discipline"
bash "$shared/selftest.sh"

# 2. Read and validate the manifest against the flows. An unparseable or empty
#    manifest, or a flow that claims no tests, must stop the run: those are
#    exactly the states in which everything downstream would pass vacuously.
smoke::load_manifest "$here/manifest.txt" "$here/flows"

# 2b. Read and validate the shard map. The union checks are the point: a flow
#     assigned to no shard, or a harness claimed by no shard, would leave both CI
#     jobs green while the smoke silently stopped running. Both run even when no
#     shard was selected, so the guard cannot be avoided by running unsharded.
smoke::load_shards "$here/shards.txt"
mapfile -t known_harnesses < <(harnesses::known)
smoke::require_shard_harness_coverage "${known_harnesses[@]}"
# ...and that every shard is actually invoked by a job. The two union checks
# above cannot see this: a shard added here but not to the workflow matrix runs
# nowhere while both legs stay green. Read-only, and skipped for a workflow whose
# job does not pass --shard, so this holds before and after the job split.
smoke::require_workflow_shards filtered-proxy-smoke \
  "$repo_root/.github/workflows/ci.yml" \
  "$repo_root/.github/workflows/release.yml" \
  "$repo_root/.github/workflows/manual-release.yml"

label="Filtered-proxy smoke"
if [[ "$shard_set" -eq 1 ]]; then
  smoke::select_shard "$shard"
  label="Filtered-proxy smoke [$shard]"
  smoke::log "Shard '$shard': flows ${SMOKE_SHARD_FLOWS[$shard]% }; harnesses ${SMOKE_SHARD_HARNESSES[$shard]% }"
else
  # No shard: run everything and install every harness, which is what this
  # entrypoint did before shards existed and still does for a local
  # --validate-only or a single-job invocation.
  SMOKE_SELECTED_HARNESSES=("${known_harnesses[@]}")
fi

if [[ "$validate_only" -eq 1 ]]; then
  echo "filtered-proxy smoke: manifest, flows and shard map validate cleanly"
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

smoke::unlock_userns
# Created unconditionally, not just by the install that populates it: CI caches
# this path for every shard, and actions/cache warns on a path that does not
# exist. A shard whose harnesses need no cached artifact should leave an empty
# directory behind, not a warning that trains reviewers to ignore cache noise on
# the leg where a real cache regression would show up.
mkdir -p "$HARNESS_CACHE_DIR"
# Only the selected shard's harnesses: a job that never launches Claude Code has
# no reason to spend a minute installing it. The version and checksum assertions
# inside each install are unchanged — see lib/harnesses.sh.
for harness in "${SMOKE_SELECTED_HARNESSES[@]}"; do
  "harnesses::install_$harness"
done

# 4. Run each flow in a subshell so a flow's trap, cwd and variables cannot leak
#    into the next one, then check its evidence.
smoke::run_flows "$label"
