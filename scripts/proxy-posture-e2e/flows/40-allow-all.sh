#!/usr/bin/env bash
# Scenario 4: ALLOW-ALL deploys neither a floor nor a proxy.
#
# An open policy with no denies asks for no distinction between destinations, so
# there is nothing to filter. The sandbox reaches the fixture DIRECTLY, which is
# what separates "no floor" from "a floor that happened to allow it".
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib/posture-fixture.sh"

flow::run() {
  smoke::trap_cleanup posture::fixture_down
  posture::fixture_up 4
  posture::go_test '^TestProxyPostureE2EAllowAllDeploysNoFloor$'
}

flow::describe() {
  cat <<'TXT'
An allow-all policy must build no floor and deploy no proxy: no proxy discovery
injected, no proxy process on the host while the sandbox runs, no decision
record, and the fixture reachable DIRECTLY from inside the sandbox — including
an address outside every authored rule.
TXT
}
