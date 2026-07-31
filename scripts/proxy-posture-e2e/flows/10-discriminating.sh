#!/usr/bin/env bash
# Scenario 1: a DISCRIMINATING allowlist deploys a proxy and enforces through it.
#
# The strongest arm of the shard: the authored destination is carried over both
# carriages, the denied one is refused with the discriminating verdict at the
# proxy's own decision record, and every route that does not go through the
# proxy — direct TCP, UDP, ICMP — is gone.
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/posture-fixture.sh"

flow::run() {
  trap posture::fixture_down EXIT
  posture::fixture_up 1
  posture::go_test '^TestProxyPostureE2EDiscriminatingAllowlist$'
}

flow::describe() {
  cat <<'TXT'
An authored allowlist must reach a real launch and be enforced by a deployed
filtering proxy: the authored destination carried over BOTH carriages, the deny
row refusing an overlapping allow with the denied_by_rule verdict recorded at
the proxy, direct TCP/UDP/ICMP refused by the floor, exactly one listening
socket in the sandbox namespace, and the preview surface predicting the proxy
mechanism that actually ran.
TXT
}
