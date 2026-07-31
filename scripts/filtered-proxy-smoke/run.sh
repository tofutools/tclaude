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

smoke::unlock_userns
harnesses::install_codex
harnesses::install_claude

# 4. Run each flow in a subshell so a flow's trap, cwd and variables cannot leak
#    into the next one, then check its evidence.
smoke::run_flows "$here" "Filtered-proxy smoke"
