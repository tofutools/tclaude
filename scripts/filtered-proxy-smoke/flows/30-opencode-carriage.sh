#!/usr/bin/env bash
# Flow: OPENCODE PROXY-CARRIAGE COOPERATION (TCL-889, the §6.2 matrix row).
#
# This flow answers ONE open question empirically: when the proxy environment a
# real proxy floor injects is present, does the OpenCode server route its model
# traffic over it, and over which carriage? OpenCode 1.18.6 carries no ALL_PROXY
# string in its binary, so SOCKS carriage was doubtful and unmeasured; a
# "not carried" answer here is a RESULT, not a failure.
#
# It is NOT floor evidence and it backs no capability cell. It runs HOST-OPEN by
# construction, because offering exactly ONE carriage per launch is the only way
# to isolate the question above — and the filtered postures cannot do that. Flow
# 40 is the floor evidence (TCL-891); this flow is the carriage isolation beside
# it, and neither replaces the other. The Go arm states the same limit at length
# in its own header.
#
# Fixture: a HOST-side dummy interface on its own subnet (198.18.4.0/24), not a
# namespace. The Go arm serves the model origin itself so it can count what
# reached the destination, and it must serve it on a NON-LOOPBACK address: a
# client that skips the proxy for loopback destinations — which is ordinary,
# documented client behavior — would otherwise be recorded as a harness that
# ignores the carriage. The subnet is disjoint from flows 10 and 20 so no flow
# can disturb another or depend on one having run.
set -euo pipefail

flow::run() {
  # NOT `local`: the EXIT trap below runs after this function has returned, so
  # a local would be out of scope by then and `set -u` would abort cleanup —
  # leaving the resolver rewritten and the interface behind.
  link=tclprxoc
  origin_addr=198.18.4.1
  origin_host=oc-model-origin.carriage.test
  probe_port=41041
  probe_pid=

  cleanup() {
    # `|| true` is not tidiness: without it, a kill that fails because socat
    # already died — one of the ways the round-trip proof below fails — aborts
    # this trap under set -e, and the interface below is never removed.
    [[ -n "${probe_pid:-}" ]] && sudo kill "$probe_pid" 2>/dev/null || true
    sudo ip link del "$link" 2>/dev/null || true
    fixture::hosts_restore
    return 0
  }
  # EXIT, never RETURN: a RETURN trap does NOT fire when set -e aborts the
  # function, which is precisely the case cleanup exists for.
  trap cleanup EXIT

  sudo ip link add "$link" type dummy
  # No kernel-generated IPv6 link-local address: this fixture is IPv4 and an
  # unasked-for address is one more thing that can answer.
  sudo ip link set dev "$link" addrgenmode none
  sudo ip link set "$link" up
  sudo ip address add "$origin_addr/24" dev "$link"
  fixture::hosts_add "$origin_addr $origin_host"

  # Anti-vacuous precondition, and it has two halves because the arm depends on
  # both. The address must answer a direct round trip — otherwise "the harness
  # reached the origin directly" could never be observed and every case would
  # record "not carried" for a fabricated reason. And the NAME must resolve to
  # that address host-side, because the proxy resolves names on the host and a
  # name that resolves nowhere would be refused as unresolvable rather than
  # carried.
  #
  # Two things this proves and two it does not, so neither is assumed silently.
  # It proves the ADDRESS and the NAME, on a port of its own: the Go arm binds
  # an ephemeral port for the real origin, so this cannot pre-prove that exact
  # listener. And it proves reachability from the HOST; that transfers to the
  # confined server because the host-open posture shares the host network
  # namespace rather than unsharing it. The remaining question — can a client
  # that cooperates actually reach the origin THROUGH the proxy — is proven
  # inside the Go arm, per carriage, before anything is measured.
  sudo socat "TCP4-LISTEN:$probe_port,bind=$origin_addr,reuseaddr,fork" \
    EXEC:/bin/cat >"$SMOKE_ARTIFACTS/opencode-carriage-probe.log" 2>&1 &
  probe_pid=$!
  sleep 1
  fixture::prove_reachable "$origin_addr" "$probe_port"
  local resolved
  resolved="$(getent hosts "$origin_host" | awk '{print $1}' | head -n1)"
  if [[ "$resolved" != "$origin_addr" ]]; then
    smoke::error "$origin_host resolves to '${resolved:-nothing}', not $origin_addr"
    return 1
  fi
  sudo kill "$probe_pid" 2>/dev/null || true
  probe_pid=

  TCLAUDE_OPENCODE_PROXY_CARRIAGE_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$SMOKE_TCLAUDE_BINARY" \
  TCLAUDE_OPENCODE_CARRIAGE_ORIGIN_ADDR="$origin_addr" \
  TCLAUDE_OPENCODE_CARRIAGE_ORIGIN_HOST="$origin_host" \
    go test ./pkg/claude/agentd \
      -run '^TestOpenCodeProxyCarriageCooperation$' \
      -count=1 -v -timeout=900s
}

flow::describe() {
  cat <<'TXT'
The carriage arm must launch the real OpenCode server through the real
agentd-owned boundary once per carriage, offering exactly one carriage of the
proxy environment a real floor injects, with the model origin reachable both
directly and through the real filtering proxy. For each carriage it records
whether the model request was carried to the proxy or went straight to the
origin, and a third launch offering NO carriage must show the proxy seeing
nothing and the origin reached directly — without that control, "not carried"
would be indistinguishable from a launch that never made a model request.

This flow backs NO capability cell. It is the carriage-isolation measurement,
not a floor: what a real floor contains is flow 40's subject, and a cooperation
result here could never have been the evidence for a rating.
TXT
}

# The per-carriage answer is the deliverable, so it is surfaced to the operator
# rather than left buried in a real server's log.
flow::report() {
  local log="$1"
  grep 'opencode-proxy-carriage: ' "$log" || echo "(no carriage record emitted)"
}
