#!/usr/bin/env bash
# Federation end-to-end smoke test: one tclaude-hub plus two real agentd
# instances ("alice" and "bob") on this host, each isolated in its own HOME
# and tmux socket directory.
#
# Bob runs a model-free `shell` agent in group "builders" and exports the
# group to alice; alice imports it and the operator sends remote mail to
# bob-shell@bob. Success = alice's outbox row reaches "accepted", which bob
# only acks after storing the message in bob-shell's inbox.
#
# Run it from a plain terminal (not from inside an agent): agentd decides
# human-vs-agent by walking the caller's process tree, so a caller with a
# harness ancestor is not the operator.
#
#   scripts/federation-smoke.sh [workdir]     # KEEP=1 leaves everything running
set -euo pipefail

ROOT=${1:-$(mktemp -d "${TMPDIR:-/tmp}/tclaude-fed-smoke.XXXXXX")}
ROOT=$(cd "$ROOT" && pwd)
BIN=$ROOT/bin
REPO=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$BIN"

log() { printf '\n== %s\n' "$*"; }

log "building into $BIN"
(cd "$REPO" && go build -o "$BIN/tclaude" . && go build -o "$BIN/tclaude-hub" ./cmd/tclaude-hub)

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
HUB_PORT=$(free_port)
HUB_URL=ws://127.0.0.1:$HUB_PORT

# inst <name> <cmd...>: run a command as that instance's operator.
# Inherited TCLAUDE_* variables (session id, resource delegation, operator
# token, ...) belong to the caller's own tclaude and must not leak in.
inst() {
	local name=$1
	shift
	local unset=(-u TMUX)
	local v
	for v in $(compgen -e | grep '^TCLAUDE_' || true); do unset+=(-u "$v"); done
	env "${unset[@]}" HOME="$ROOT/$name" TMUX_TMPDIR="$ROOT/$name/tmux" PATH="$BIN:$PATH" "$@"
}

PIDS=()
cleanup() {
	if [[ "${KEEP:-}" == 1 ]]; then
		echo "KEEP=1: leaving hub and daemons running under $ROOT (pids ${PIDS[*]})"
		return
	fi
	for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done
	for n in alice bob; do inst "$n" tmux -L tclaude kill-server 2>/dev/null || true; done
	wait 2>/dev/null || true
	echo "logs kept in $ROOT"
}
trap cleanup EXIT

wait_for() { # wait_for <desc> <cmd...>
	local desc=$1
	shift
	for _ in $(seq 1 150); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.2
	done
	echo "TIMEOUT waiting for: $desc" >&2
	return 1
}

log "starting hub on $HUB_URL"
mkdir -p "$ROOT/hub"
TCLAUDE_HUB_DIR=$ROOT/hub "$BIN/tclaude-hub" serve --listen "127.0.0.1:$HUB_PORT" --policy-refresh 1s >"$ROOT/hub/hub.log" 2>&1 &
PIDS+=($!)

for n in alice bob; do
	log "starting agentd for $n"
	mkdir -p "$ROOT/$n/tmux"
	inst "$n" "$BIN/tclaude" agentd serve --no-tray --persist-operator-token --no-print-human-token >"$ROOT/$n/agentd.log" 2>&1 &
	PIDS+=($!)
done
for n in alice bob; do
	wait_for "$n agentd socket" test -S "$ROOT/$n/.tclaude/api/agentd-socket/agentd.sock"
	wait_for "$n operator token" test -s "$ROOT/$n/.tclaude/data/operator_token"
done

ALICE_ID=$(inst alice tclaude federation identity 2>/dev/null)
BOB_ID=$(inst bob tclaude federation identity 2>/dev/null)
echo "alice=$ALICE_ID bob=$BOB_ID"

log "admitting both on the hub"
TCLAUDE_HUB_DIR=$ROOT/hub "$BIN/tclaude-hub" admit "$ALICE_ID"
TCLAUDE_HUB_DIR=$ROOT/hub "$BIN/tclaude-hub" admit "$BOB_ID"

log "connecting"
inst alice tclaude federation connect "$HUB_URL" --name alice
inst bob tclaude federation connect "$HUB_URL" --name bob

sees() { inst "$1" tclaude federation peers --json | grep -q "$2"; }
wait_for "alice sees bob" sees alice "$BOB_ID"
wait_for "bob sees alice" sees bob "$ALICE_ID"
inst alice tclaude federation peers

log "trusting"
inst alice tclaude federation trust "$BOB_ID" --label bob
inst bob tclaude federation trust "$ALICE_ID" --label alice

log "bob: shell agent in group builders, exported to alice"
inst bob tclaude agent groups create builders
inst bob tclaude agent spawn builders --harness shell --name bob-shell
inst bob tclaude federation export builders --to alice --cap roster,presence,mail

catalog_has() { inst alice tclaude federation remote | grep -q "bob-shell@bob"; }
wait_for "alice receives bob's catalog" catalog_has
inst alice tclaude federation remote

log "alice: import and send"
inst alice tclaude agent groups create team
inst alice tclaude federation import bob/builders --into team
inst alice tclaude federation send bob-shell@bob "hello from alice's operator" --subject smoke

accepted() { inst alice tclaude federation outbox --json | grep -q '"state": "accepted"'; }
wait_for "alice's mail accepted by bob" accepted
inst alice tclaude federation outbox
inst bob tclaude federation status

log "PASS: remote mail delivered and acknowledged"
