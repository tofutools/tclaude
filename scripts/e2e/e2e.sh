#!/usr/bin/env bash
# Manual E2E environment: one tclaude-hub plus two isolated tclaude nodes ("a"
# and "b") on this host, driven through their dashboards and CLIs. Claude Code
# agents talk to a local mock of the Messages API, so no credentials or tokens
# are needed. See docs/e2e-manual-verification.md for the scenario checklist.
#
#   e2e.sh build                 build tclaude, tclaude-hub and drive into $BIN
#   e2e.sh up | down | status    start / stop the hub, both nodes and their mock APIs
#   e2e.sh pair                  connect both nodes to the hub and trust each other
#   e2e.sh run <a|b> <args...>   run tclaude as that node's operator
#   e2e.sh hub <args...>         run tclaude-hub against the hub's data dir
#   e2e.sh dash <a|b>            print that node's dashboard login URL
#   e2e.sh browser start|stop    headless Chrome for scripts/e2e/drive (port $DRIVE_PORT)
set -euo pipefail

REPO=$(cd "$(dirname "$0")/../.." && pwd)
HERE=$REPO/scripts/e2e
BASE=$(realpath -m "${E2E_BASE:-${TMPDIR:-/tmp}/tce2e}") # keep SHORT: unix socket paths are limited to ~108 bytes
BIN=$(realpath -m "${E2E_BIN:-$BASE/bin}")
HUB_ADDR=127.0.0.1:18470
declare -A PORT=([a]=18481 [b]=18482)
declare -A MOCK=([a]=18491 [b]=18492)
export DRIVE_PORT=${DRIVE_PORT:-19222}

# Start every process from an allowlisted environment. A calling agent leaks
# variables (TCLAUDE_AGENTD_SOCKET, CLAUDE_CONFIG_DIR, TMUX, ...) that would
# point the nodes at the operator's real daemon, tmux server or credentials.
clean_env() {
	${E2E_EXEC:-} env -i PATH="$BIN:$PATH" TERM="${TERM:-xterm-256color}" LANG="${LANG:-C.UTF-8}" \
		USER="${USER:-$(id -un)}" LOGNAME="${LOGNAME:-$(id -un)}" SHELL="${SHELL:-/bin/bash}" \
		TMPDIR="${TMPDIR:-/tmp}" "$@"
}

node_env() { # node_env <a|b> cmd...
	local n=$1
	shift
	clean_env HOME="$BASE/$n" TMUX_TMPDIR="$BASE/$n/tmux" XDG_CONFIG_HOME="$BASE/$n/.config" \
		XDG_CACHE_HOME="$BASE/$n/.cache" XDG_DATA_HOME="$BASE/$n/.local/share" \
		ANTHROPIC_BASE_URL="http://127.0.0.1:${MOCK[$n]}" ANTHROPIC_API_KEY="sk-ant-e2e-mock-key-0000000000" \
		CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 DISABLE_AUTOUPDATER=1 "$@"
}

hub_env() { clean_env HOME="$BASE/hub" TCLAUDE_HUB_DIR="$BASE/hub/data" "$@"; }

# alive <name>: the pid file exists and still names one of our processes.
alive() {
	local f=$BASE/$1.pid pid
	[[ -f $f ]] || return 1
	pid=$(cat "$f")
	kill -0 "$pid" 2>/dev/null && tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null | grep -qE "$BASE|$BIN|mockapi.py"
}

# Claude Code onboarding, API-key approval (last 20 chars of the key) and
# folder trust are pre-accepted so agents start without interactive prompts.
seed_claude() { # seed_claude <a|b>
	local n=$1
	mkdir -p "$BASE/$n/.claude"
	# Spawned agents read ~/.claude/.claude.json; a bare `claude` reads ~/.claude.json.
	python3 - "$BASE/$n" "$BASE/$n/proj" <<'EOF'
import json, os, sys
home, *dirs = sys.argv[1:]
for path in (f"{home}/.claude.json", f"{home}/.claude/.claude.json"):
    d = json.load(open(path)) if os.path.exists(path) else {}
    d.setdefault("hasCompletedOnboarding", True)
    d.setdefault("theme", "dark")
    d.setdefault("customApiKeyResponses", {"approved": ["-mock-key-0000000000"], "rejected": []})
    for p in [home, *dirs]:
        d.setdefault("projects", {}).setdefault(p, {})["hasTrustDialogAccepted"] = True
    json.dump(d, open(path, "w"))
EOF
}

case ${1:-} in
build)
	mkdir -p "$BIN"
	(cd "$REPO" && go build -o "$BIN/" . ./cmd/... && go build -o "$BIN/drive" ./scripts/e2e/drive)
	ls "$BIN"
	;;
up)
	for x in hub a b mock-a mock-b; do
		if alive "$x"; then echo "$x is already running; run '$0 down' first" >&2 && exit 1; fi
	done
	mkdir -p "$BASE"/{hub/data,logs} "$BASE"/{a,b}/{tmux,proj}
	(cd "$BASE/hub" && E2E_EXEC=exec hub_env nohup "$BIN/tclaude-hub" serve --listen "$HUB_ADDR" >"$BASE/logs/hub.log" 2>&1 &
		echo $! >"$BASE/hub.pid")
	for n in a b; do
		seed_claude "$n"
		(exec nohup python3 "$HERE/mockapi.py" "${MOCK[$n]}" "node-$n" >"$BASE/logs/mock-$n.log" 2>&1) &
		echo $! >"$BASE/mock-$n.pid"
		(cd "$BASE/$n" && E2E_EXEC=exec node_env "$n" nohup "$BIN/tclaude" agentd serve --no-tray --persist-operator-token \
			--dashboard-port "${PORT[$n]}" >"$BASE/logs/node-$n.log" 2>&1 &
			echo $! >"$BASE/$n.pid")
	done
	sleep 3
	"$0" status
	echo "hub admin claim token: $(grep -o 'tchac_[0-9a-f]*' "$BASE/logs/hub.log" | head -1 || true) (see logs/hub.log)"
	;;
pair)
	for n in a b; do
		"$0" run "$n" federation connect "ws://$HUB_ADDR" --invite "$("$0" hub invite)" --name "node-$n"
	done
	sleep 3
	ida=$("$0" run a federation identity | head -1)
	idb=$("$0" run b federation identity | head -1)
	"$0" run a federation trust "$idb" --level unrestricted --yes --label b
	"$0" run b federation trust "$ida" --level unrestricted --yes --label a
	"$0" run a federation peers
	;;
down)
	for x in hub a b mock-a mock-b; do
		if alive "$x"; then kill "$(cat "$BASE/$x.pid")"; fi
		rm -f "$BASE/$x.pid"
	done
	for n in a b; do node_env "$n" tmux -L tclaude kill-server 2>/dev/null || true; done
	"$0" browser stop
	echo down
	;;
status)
	for x in hub a b mock-a mock-b chrome; do
		if alive "$x"; then echo "$x: up (pid $(cat "$BASE/$x.pid"))"; else echo "$x: down"; fi
	done
	;;
run)
	n=$2
	shift 2
	cd "$BASE/$n" && node_env "$n" "$BIN/tclaude" "$@"
	;;
hub)
	shift
	cd "$BASE/hub" && hub_env "$BIN/tclaude-hub" "$@"
	;;
dash) "$0" run "$2" agent dashboard --print | grep -o 'http[^ ]*' | head -1 ;;
browser)
	case ${2:-} in
	start)
		if alive chrome; then echo "chrome is already running" >&2 && exit 1; fi
		# Chrome's crashpad ignores --user-data-dir and aborts when ~/.config is
		# unwritable (agent sandboxes), so point every XDG dir at a disposable one.
		c=$BASE/chrome
		mkdir -p "$c"/{config,cache,data,profile}
		(XDG_CONFIG_HOME=$c/config XDG_CACHE_HOME=$c/cache XDG_DATA_HOME=$c/data exec nohup google-chrome --headless=new \
			--no-sandbox --disable-gpu --hide-scrollbars --window-size=1280,800 --remote-debugging-port="$DRIVE_PORT" \
			--user-data-dir="$c/profile" about:blank >"$BASE/logs/chrome.log" 2>&1) &
		echo $! >"$BASE/chrome.pid"
		sleep 2
		echo "chrome on :$DRIVE_PORT (drive with $BIN/drive)"
		;;
	stop)
		if alive chrome; then kill "$(cat "$BASE/chrome.pid")"; fi
		rm -f "$BASE/chrome.pid"
		;;
	*) echo "usage: $0 browser start|stop" >&2 && exit 2 ;;
	esac
	;;
*)
	sed -n '2,13p' "$0" >&2
	exit 2
	;;
esac
