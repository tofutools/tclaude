#!/usr/bin/env bash
# Entrypoint for the END-TO-END proxy-posture smokes (M2.6).
#
# This shard answers one question the per-milestone smokes do not: does the
# ASSEMBLED posture behave as authored? Authored policy in, real launch, real
# enforcement observed — for each of the four postures the deployment table
# distinguishes, including the three that must deploy NO proxy.
#
# It is a SEPARATE shard from scripts/filtered-proxy-smoke/ on purpose. The
# operator asked for one dedicated, visible verification rather than a check
# folded into an existing job, and the separation also keeps the two shards'
# host prerequisites honest: the proxy smokes deliberately install no pasta and
# no nftables, and this one does — see flows/30-loopback-only.sh for why.
#
# The evidence discipline is shared (scripts/lib/smoke/), so a skipped, renamed,
# filtered-out or zero-test smoke is a hard failure here in exactly the way it
# is there.
#
# NEVER RUN THIS LOCALLY. The flows build real bubblewrap sandboxes, create
# network namespaces with sudo, and temporarily rewrite /etc/hosts.
#
# --validate-only runs the self-test and every manifest/flow consistency check
# and then stops, touching no sandbox and no network.
set -euo pipefail

validate_only=0
case "${1:-}" in
  --validate-only) validate_only=1 ;;
  "")              validate_only=0 ;;
  *)
    printf 'usage: %s [--validate-only]\n' "${BASH_SOURCE[0]}" >&2
    printf 'unrecognized argument: %s\n' "$1" >&2
    # The destructive path is the DEFAULT, so a typo like --validate or
    # --dry-run must not fall through to it.
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
# shellcheck source=../lib/smoke/packet-floor.sh
source "$shared/packet-floor.sh"
# shellcheck source=lib/prereqs.sh
source "$here/lib/prereqs.sh"

export SMOKE_ARTIFACTS="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/proxy-posture-e2e"
export SMOKE_HOSTS_BACKUP="$SMOKE_ARTIFACTS/etc-hosts.before"
export SMOKE_TCLAUDE_BINARY="$SMOKE_ARTIFACTS/tclaude"
mkdir -p "$SMOKE_ARTIFACTS"

# 1. Prove the evidence checker and the manifest guards BEFORE trusting any
#    result they produce.
smoke::log "Self-testing the shared evidence discipline"
bash "$shared/selftest.sh"

# 2. Read and validate the manifest against the flows.
smoke::load_manifest "$here/manifest.txt" "$here/flows"

if [[ "$validate_only" -eq 1 ]]; then
  echo "proxy-posture e2e: manifest and flows validate cleanly"
  exit 0
fi

# Past this point the script is destructive.
if [[ "${CI:-}" != "true" && "${TCLAUDE_ALLOW_LOCAL_PROXY_SMOKE:-}" != "1" ]]; then
  smoke::error "refusing to run the real smokes outside CI"
  cat >&2 <<'TXT'
This builds real bubblewrap sandboxes and temporarily rewrites /etc/hosts. Run
one of these instead:

  scripts/proxy-posture-e2e/run.sh --validate-only   # manifest + flow checks
  scripts/proxy-posture-e2e/selftest.sh              # evidence checker

Set TCLAUDE_ALLOW_LOCAL_PROXY_SMOKE=1 only if you genuinely mean it.
TXT
  exit 2
fi

# Whatever happens from here, the runner's resolver goes back. Flows restore it
# too; this is the backstop for a cancelled job, which fires no flow trap.
trap fixture::hosts_restore EXIT INT TERM

# 3. Prerequisites. `pasta` and `nft` are deliberately NOT here: only the
#    loopback-only flow needs them, and it installs them itself so a passt build
#    break cannot fail the three scenarios that never touch the packet floor —
#    nor read as a posture regression.
prereqs::install
smoke::require_command go sudo ip socat bwrap git || exit 1

smoke::log "Building tclaude for the posture launches"
go build -o "$SMOKE_TCLAUDE_BINARY" .

smoke::unlock_userns

# 4. Run each flow in a subshell so a flow's trap, cwd and variables cannot leak
#    into the next one, then check its evidence.
smoke::run_flows "Proxy-posture e2e"
