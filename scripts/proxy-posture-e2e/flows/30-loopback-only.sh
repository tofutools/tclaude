#!/usr/bin/env bash
# Scenario 3: a LOOPBACK-ONLY list deploys no proxy.
#
# This is the conditional-deployment scenario, and the one flow in this shard
# that needs the PACKET floor. A loopback-only list is a filtered posture that is
# not discriminating, so no engine deploys and the launch runs the floor the
# packet gateway builds — which needs a trusted `pasta` and `nft`.
#
# # Why the prerequisite is installed HERE
#
# Two reasons, both about attribution:
#
#   1. An upstream passt build break, a moved release tag or an untrusted runner
#      path must not fail the three scenarios that never touch the packet floor.
#   2. It must never READ as a posture regression. smoke::packet_floor_install
#      reports every failure as a prerequisite verdict, with its own step-summary
#      block saying that no policy was evaluated — so "pasta would not build" and
#      "the sandbox stopped enforcing the posture" can never be confused.
#
# The proxy smokes' deliberate no-pasta claim is unaffected: that claim is about
# what THOSE flows call, and they still call neither.
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/posture-fixture.sh"

flow::run() {
  # The prerequisite comes first and speaks for itself on failure. Nothing has
  # been created yet at this point, so an early return needs no cleanup.
  smoke::packet_floor_install || return 1

  trap posture::fixture_down EXIT
  posture::fixture_up 3
  posture::go_test '^TestProxyPostureE2ELoopbackOnlyDeploysNoProxy$'
}

flow::describe() {
  cat <<'TXT'
A loopback-only list must deploy NO filtering proxy while still being enforced:
no proxy process on the host while the sandbox runs, no listening socket at all
in the sandbox's own network namespace (where a deployed proxy's listener would
be), no proxy discovery injected, no decision record — and the authored host
loopback port reachable through the floor while an unauthored one is refused.

If this flow failed with a "packet-floor prerequisite failed" message instead,
the posture was never exercised: pasta/nft could not be provisioned, which is a
host problem rather than an enforcement one.
TXT
}
