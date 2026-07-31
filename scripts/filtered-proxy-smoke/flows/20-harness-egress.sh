#!/usr/bin/env bash
# Flow: pinned HARNESS COOPERATION and TOOL EGRESS (§8.1 tests 4-5).
#
# Fixture: its own namespace on its own subnet (198.18.3.0/24), independent of
# the floor/policy flow above, so neither can disturb the other and neither
# depends on the order they run in.
set -euo pipefail

flow::run() {
  local ns=tclaude-proxy-cooperation
  local host_link=tclprx2 peer_link=tclprx3
  local allowed=198.18.3.10 adjacent=198.18.3.200
  # 443 carries the model origins and is NOT configurable: the pinned harnesses
  # choose that port themselves, so the Go smoke pins it too and the fixture
  # merely has to answer there.
  local -a ports=(443 41021 41022)
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

  fixture::netns_up "$ns" "$host_link" "$peer_link" 198.18.3.1/24 \
    "$allowed/24" "$adjacent/24"

  # The pinned harnesses' REAL model origins are pointed at the fixture for the
  # duration of this flow, and /etc/hosts is restored on the way out. That is
  # what keeps the smoke OFFLINE: no packet can reach a real model provider, so
  # the deliberately invalid credentials cannot even be presented to one.
  fixture::hosts_add \
    "$allowed allowed.proxy.tclaude.test" \
    "$allowed api.anthropic.com api.openai.com"

  local port
  for port in "${ports[@]}"; do
    pids+=("$(fixture::serve "$ns" "$allowed" "$port" \
      "$SMOKE_ARTIFACTS/harness-egress-tcp-$port.log")")
  done
  sleep 1
  for port in "${ports[@]}"; do
    fixture::prove_reachable "$allowed" "$port"
  done

  TCLAUDE_FILTERED_PROXY_SMOKE=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$SMOKE_TCLAUDE_BINARY" \
  TCLAUDE_FILTERED_ALLOWED_ADDR="$allowed" \
  TCLAUDE_FILTERED_ADJACENT_ADDR="$adjacent" \
  TCLAUDE_FILTERED_ALLOWED_PREFIX=198.18.3.0/25 \
  TCLAUDE_FILTERED_ALLOWED_PORT=41021 \
  TCLAUDE_FILTERED_DENIED_PORT=41022 \
    go test ./pkg/claude/session \
      -run '^TestPinnedProxy(HarnessCooperation|ToolEgress)$' \
      -count=1 -v -timeout=900s
}

flow::describe() {
  cat <<'TXT'
The cooperation smoke must run the pinned harnesses inside the real proxy floor
on deliberately invalid credentials, observe each expected model origin carried
AT THE PROXY, observe the deliberate undeclared probe refused over both
carriages, and show no undeclared origin allowed. The tool-egress smoke must run
curl over BOTH carriages, git over HTTPS and a go module fetch inside the floor,
with allowed destinations carried and LIVE denied destinations failing closed at
the proxy.

The Linux engine:proxy capability cells are backed by this flow. A failure here
means those cells have lost their evidence.
TXT
}

# Per-harness carriage is the empirical answer to the ALL_PROXY question, so it
# is surfaced to the operator rather than left buried in the log.
flow::report() {
  local log="$1"
  grep 'proxy-cooperation: ' "$log" || echo "(no carriage record emitted)"
}
