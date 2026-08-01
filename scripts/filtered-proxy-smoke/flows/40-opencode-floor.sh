#!/usr/bin/env bash
# Flow: OPENCODE BEHIND THE REAL PROXY FLOOR (TCL-891, the §6.2 matrix row).
#
# THIS IS FLOOR EVIDENCE, and it is what flow 30 could not be. Flow 30 measured
# OpenCode's carriage cooperation under the HOST-OPEN posture because the
# agentd-owned Unix-relay boundary refused to deploy the proxy engine at all.
# TCL-891 generalized the inherited-descriptor contract to the proxy supervisor,
# so the same real boundary can now be launched behind the real floor: an empty
# network namespace with the supervised filtering proxy as its only exit.
#
# The two flows are complementary and neither replaces the other. Flow 30 offers
# ONE carriage per launch, which is the only construction that can answer "does
# this harness ignore ALL_PROXY". This flow offers BOTH, exactly as production
# does, and its subject is the floor: what is contained, what is refused, and
# whether anything reached the origin that the proxy never decided about.
#
# Fixture: a HOST-side dummy interface on its own subnet (198.18.5.0/24), not a
# namespace. Disjoint from flows 10, 20 and 30 so no flow can disturb another or
# depend on one having run. Four names resolve to the one fixture address, and
# each has a job:
#
#   origin      — authored ALLOW; the model origin the Go arm serves itself,
#                 and the ONLY name whose decisions are read as OpenCode's
#   declared    — authored ALLOW; the in-floor probe's carried destination. It
#                 is a separate name from the origin precisely so the probe's
#                 own allowed decisions can never be mistaken for the server's
#   undeclared  — authored NOWHERE; refused as `not_authorized`
#   denied      — authored ALLOW *and* DENY; refused as `denied_by_rule`
#
# The undeclared and denied names deliberately RESOLVE to a live address. A name
# that resolved nowhere would be refused as unresolvable, which is the resolver
# failing rather than the policy answering — and the §5.2 deny evidence would be
# about nothing.
#
# NO PACKET-FLOOR PREREQUISITES. The proxy floor needs bubblewrap and pidfds and
# nothing else; this job installs neither pasta nor nft, and a green run here is
# itself part of the evidence that the proxy engine's floor does not reach for
# them.
set -euo pipefail

flow::run() {
  # NOT `local`: the EXIT trap below runs after this function has returned, so
  # a local would be out of scope by then and `set -u` would abort cleanup —
  # leaving the resolver rewritten and the interface behind.
  link=tclprxfl
  origin_addr=198.18.5.1
  origin_host=oc-floor-origin.floor.test
  undeclared_host=oc-floor-undeclared.floor.test
  denied_host=oc-floor-denied.floor.test
  declared_host=oc-floor-declared.floor.test
  probe_port=41051
  probe_pid=
  arm_log="$SMOKE_ARTIFACTS/opencode-floor-arm.log"

  cleanup() {
    # smoke::kill_listener tolerates an already-dead or never-started listener
    # — one of the ways the round-trip proof below fails — so a kill cannot
    # abort this trap under set -e and leave the interface behind. It signals
    # socat itself, not only the sudo wrapper holding it.
    smoke::kill_listener "${probe_pid:-}"
    sudo ip link del "$link" 2>/dev/null || true
    fixture::hosts_restore
    return 0
  }
  # EXIT, never RETURN: a RETURN trap does NOT fire when set -e aborts the
  # function, which is precisely the case cleanup exists for. INT and TERM are
  # covered too (smoke::trap_cleanup): bash runs the EXIT trap on a fatal
  # signal, so what those add is the FAILURE — an interrupted flow would
  # otherwise exit 0 and be judged a pass.
  smoke::trap_cleanup cleanup

  sudo ip link add "$link" type dummy
  # No kernel-generated IPv6 link-local address: this fixture is IPv4 and an
  # unasked-for address is one more thing that can answer.
  sudo ip link set dev "$link" addrgenmode none
  sudo ip link set "$link" up
  sudo ip address add "$origin_addr/24" dev "$link"
  fixture::hosts_add \
    "$origin_addr $origin_host $declared_host $undeclared_host $denied_host"

  # Anti-vacuous precondition. The address must answer a direct round trip, and
  # all four names must resolve to it host-side — the proxy resolves names on
  # the HOST, so a name that resolved nowhere would produce an unresolvable
  # verdict rather than the policy verdict each name exists to demonstrate.
  #
  # What this proves and what it does not, so neither is assumed silently. It
  # proves the ADDRESS and the NAMES on a port of its own; the Go arm binds an
  # ephemeral port for the real origin, so this cannot pre-prove that exact
  # listener. Reachability THROUGH the proxy, per carriage, is proven inside the
  # floor by the arm's own in-namespace probe before anything is measured.
  sudo socat "TCP4-LISTEN:$probe_port,bind=$origin_addr,reuseaddr,fork" \
    EXEC:/bin/cat >"$SMOKE_ARTIFACTS/opencode-floor-probe.log" 2>&1 &
  probe_pid=$!
  sleep 1
  fixture::prove_reachable "$origin_addr" "$probe_port"
  local name resolved
  for name in "$origin_host" "$declared_host" "$undeclared_host" "$denied_host"; do
    resolved="$(getent hosts "$name" | awk '{print $1}' | head -n1)"
    if [[ "$resolved" != "$origin_addr" ]]; then
      smoke::error "$name resolves to '${resolved:-nothing}', not $origin_addr"
      return 1
    fi
  done
  smoke::kill_listener "$probe_pid"
  probe_pid=

  TCLAUDE_OPENCODE_PROXY_FLOOR_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$SMOKE_TCLAUDE_BINARY" \
  TCLAUDE_OPENCODE_FLOOR_ORIGIN_ADDR="$origin_addr" \
  TCLAUDE_OPENCODE_FLOOR_ORIGIN_HOST="$origin_host" \
  TCLAUDE_OPENCODE_FLOOR_UNDECLARED_HOST="$undeclared_host" \
  TCLAUDE_OPENCODE_FLOOR_DENIED_HOST="$denied_host" \
  TCLAUDE_OPENCODE_FLOOR_DECLARED_HOST="$declared_host" \
    go test ./pkg/claude/agentd \
      -run '^TestOpenCodeProxyFloorCooperation$' \
      -count=1 -v -timeout=900s | tee "$arm_log"

  # THE PROBE MARKERS ARE COUNTED HERE, NOT IN flow::report. The driver calls
  # flow::report only after a flow has already been marked passed, and it
  # discards the function's status — a check there would read like a gate and
  # gate nothing. This runs inside flow::run, where a non-zero return fails the
  # flow.
  #
  # Four is the whole set: the undeclared and deny-row questions over each of
  # the two carriages. Fewer means the in-floor probe did not ask them all,
  # which is exactly the state in which the arm's refusal evidence would be
  # about a record nothing wrote. The Go arm asserts the same set itself; this
  # is the shell-side backstop against an arm that stopped emitting them.
  local refusals
  refusals="$(grep -c 'opencode-proxy-floor-probe: .*: refused' "$arm_log" || true)"
  if [[ "${refusals:-0}" -ne 4 ]]; then
    smoke::error "in-floor probe recorded ${refusals:-0} refusals, expected 4; the refusal evidence is incomplete"
    return 1
  fi
  smoke::log "in-floor probe refusals recorded: $refusals"
}

# The arm launches the real OpenCode server behind the real floor:
# opencode_proxy_floor_smoke_linux_test.go resolves it through
# harness.OpenCodeExecutable() and wraps it before anything is measured.
flow::harnesses() {
  echo opencode
}

flow::describe() {
  cat <<'TXT'
The floor arm must launch the real OpenCode server through the real
agentd-owned Unix-relay boundary behind the real proxy floor: an empty network
namespace, both carriages injected by the production launcher, deliberately
invalid credentials, and the supervisor's own filtering proxy as the only exit.

Inside that namespace, before OpenCode is measured, a cooperating-client probe
asks one declared and two refused questions over EACH carriage. Its per-carriage
markers must all be present — a probe that silently failed to run would leave a
decision record containing only what OpenCode happened to do, and the refusal
evidence would be satisfied by its own absence.

What a green run establishes: undeclared and deny-row destinations refused over
both carriages with distinct verdicts, the declared destination carried over
both, and no model request completing at the origin that the proxy did not
decide about — the last being the floor's structural guarantee rather than a
count.

This flow does NOT by itself activate any capability cell. The activation record
and the ratings that follow it are made in the PR that cites this flow's green
named run.
TXT
}

# The floor answer is the deliverable, so it is surfaced to the operator rather
# than left buried in a real server's log. The probe markers are reported too:
# they are what says the refusal evidence was produced rather than skipped.
flow::report() {
  local log="$1"
  grep 'opencode-proxy-floor: ' "$log" || echo "(no floor record emitted)"
  # Display only. The gate that MATTERS is in flow::run, because the driver
  # discards this function's status.
  grep 'opencode-proxy-floor-probe: ' "$log" || echo "(no probe markers emitted)"
}
