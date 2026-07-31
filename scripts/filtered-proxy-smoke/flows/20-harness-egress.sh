#!/usr/bin/env bash
# Flow: pinned HARNESS COOPERATION and TOOL EGRESS (§8.1 tests 4-5).
#
# Fixture: its own namespace on its own subnet (198.18.3.0/24), independent of
# the floor/policy flow above, so neither can disturb the other and neither
# depends on the order they run in.
set -euo pipefail

flow::run() {
  # NOT `local`: the EXIT trap below runs after this function has returned, so
  # a local would be out of scope by then and `set -u` would abort cleanup —
  # leaving the resolver rewritten and the namespace behind, which is the very
  # failure the trap exists to prevent. flow::run owns its subshell, so plain
  # assignment leaks nothing.
  ns=tclaude-proxy-cooperation
  host_link=tclprx2
  peer_link=tclprx3
  local allowed=198.18.3.10 adjacent=198.18.3.200
  # 443 carries the model origins and is NOT configurable: the pinned harnesses
  # choose that port themselves, so the Go smoke pins it too and the fixture
  # merely has to answer there.
  # Derived below rather than repeated: flow 10 already does this, and a
  # renumbered array with stale exports would hand the Go smoke a port with no
  # listener — a fabricated failure of exactly the kind this suite must not
  # produce.
  local -a ports=(443 41021 41022)
  pids=()

  cleanup() {
    smoke::kill_listener "${pids[@]:-}"
    fixture::netns_down "$ns" "$host_link"
    fixture::hosts_restore
  }
  # EXIT, never RETURN: a RETURN trap does NOT fire when set -e aborts the
  # function, which is precisely the case cleanup exists for. run.sh calls
  # flow::run inside a subshell, so EXIT fires when that subshell ends. INT and
  # TERM are taken as well (smoke::trap_cleanup) not because the teardown would
  # otherwise be skipped — bash runs the EXIT trap on a fatal signal too — but
  # because an interrupted flow would otherwise exit 0 and be judged a pass.
  smoke::trap_cleanup cleanup

  fixture::netns_up "$ns" "$host_link" "$peer_link" 198.18.3.1/24 \
    "$allowed/24" "$adjacent/24"

  # The pinned harnesses' REAL model origins are pointed at the fixture for the
  # duration of this flow, and /etc/hosts is restored on the way out. That is
  # what keeps the smoke OFFLINE: no packet can reach a real model provider, so
  # the deliberately invalid credentials cannot even be presented to one.
  # The go arm addresses its fixture by NAME because cmd/go refuses to proxy a
  # loopback literal; the name has to resolve host-side, which is where the
  # proxy resolves it.
  fixture::hosts_add \
    "$allowed allowed.proxy.tclaude.test" \
    "$allowed api.anthropic.com api.openai.com" \
    "127.0.0.1 egress.proxy.tclaude.test"

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
  TCLAUDE_FILTERED_ALLOWED_PORT="${ports[1]}" \
  TCLAUDE_FILTERED_DENIED_PORT="${ports[2]}" \
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
