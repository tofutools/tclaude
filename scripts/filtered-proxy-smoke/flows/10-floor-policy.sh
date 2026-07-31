#!/usr/bin/env bash
# Flow: the proxy engine's FLOOR and its POLICY ENGINE (§8.1 tests 1-2).
#
# Adding a flow is a repo edit: drop a file in this directory, declare the
# tests it must produce in ../manifest.txt, and the entrypoint picks it up.
# No workflow change is involved, which is the reason this layout exists.
#
# Fixture: its own namespace on its own subnet, independent of every other
# flow. 198.18.2.0/25 covers the allowed address and NOT the adjacent one, so
# the adjacent address is both "outside the authored CIDR" and inside the
# benchmarking space the private-destination blocker refuses.
set -euo pipefail

flow::run() {
  local ns=tclaude-proxy-target
  local host_link=tclprx0 peer_link=tclprx1
  local allowed=198.18.2.10 adjacent=198.18.2.200
  local -a ports=(41011 41012)
  local -a pids=()

  cleanup() {
    local pid
    for pid in "${pids[@]:-}"; do sudo kill "$pid" 2>/dev/null || true; done
    fixture::netns_down "$ns" "$host_link"
    fixture::hosts_restore
  }
  # EXIT, never RETURN: a RETURN trap does NOT fire when set -e aborts the
  # function, which is precisely the case cleanup exists for. run.sh calls
  # flow::run inside a subshell, so EXIT fires when that subshell ends however
  # it ends.
  trap cleanup EXIT

  fixture::netns_up "$ns" "$host_link" "$peer_link" 198.18.2.1/24 \
    "$allowed/24" "$adjacent/24"

  # The proxy resolves names host-side, so the fixture names live in the HOST's
  # resolver. allowed/sibling/denied deliberately share ONE address: what
  # separates them is authored name identity, not reachability, which is what
  # makes refusing an unauthored name meaningful. The private name points at
  # the address outside the authored CIDR, so its refusal comes from the
  # resolved answer rather than from the name being unauthorized.
  fixture::hosts_add \
    "$allowed allowed.proxy.tclaude.test sibling.proxy.tclaude.test denied.proxy.tclaude.test" \
    "$adjacent private.proxy.tclaude.test"

  local address port
  for address in "$allowed" "$adjacent"; do
    for port in "${ports[@]}"; do
      pids+=("$(fixture::serve "$ns" "$address" "$port" \
        "$SMOKE_ARTIFACTS/floor-policy-tcp-$address-$port.log")")
    done
  done
  sleep 1
  for address in "$allowed" "$adjacent"; do
    for port in "${ports[@]}"; do
      fixture::prove_reachable "$address" "$port"
    done
  done

  TCLAUDE_FILTERED_PROXY_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$SMOKE_TCLAUDE_BINARY" \
  TCLAUDE_FILTERED_ALLOWED_ADDR="$allowed" \
  TCLAUDE_FILTERED_ADJACENT_ADDR="$adjacent" \
  TCLAUDE_FILTERED_ALLOWED_PREFIX=198.18.2.0/25 \
  TCLAUDE_FILTERED_ALLOWED_PORT="${ports[0]}" \
  TCLAUDE_FILTERED_DENIED_PORT="${ports[1]}" \
    go test ./pkg/claude/session \
      -run '^TestTclaudeLayerProxy(Floor|Policy)Smoke$' \
      -count=1 -v -timeout=300s
}

flow::describe() {
  cat <<'TXT'
The floor must show direct TCP, UDP, DNS, ICMP and local name resolution all
refused inside the launched sandbox, with only the proxy port answering. The
policy engine must execute every case over BOTH the HTTP CONNECT and SOCKS5
carriages through a real launch, including sibling-name, port-narrowing,
deny-wins, CIDR-literal, resolved-private, host-loopback and
no-upstream-chaining cases.
TXT
}
