#!/usr/bin/env bash
# Scenario 2: an OPEN baseline with a deny row.
#
# Open plus denies is discriminating, so a proxy deploys. What this flow exists
# to execute is the §4.4 amended ruling: the private-destination blocker does
# NOT apply under an open baseline, so private space stays reachable while the
# authored deny is still enforced.
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/posture-fixture.sh"

flow::run() {
  smoke::trap_cleanup posture::fixture_down
  posture::fixture_up 2
  posture::go_test '^TestProxyPostureE2EOpenBaselineWithDeny$'
}

flow::describe() {
  cat <<'TXT'
Under an open baseline with one deny row: the deny must be observed executing
with the denied_by_rule verdict, while an RFC1918 name, an RFC1918 literal and a
reserved-space literal are all CARRIED — the amended §4.4 ruling. Host loopback
must still be refused as a private destination, because reaching the host always
requires an authored loopback row.
TXT
}
