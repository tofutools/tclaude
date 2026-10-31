#!/usr/bin/env bash
# OpenCode group import smoke: a real OpenCode, two isolated tclaude nodes, and
# no model call.
#
# Node "src" imports a native session into OpenCode's own store, puts it in a
# group and exports the group. Node "dst", with an empty OpenCode store, dry-runs
# and imports that archive, then resumes the group. The run passes when the
# conversation is in dst's store, OpenCode exports it, the managed server serves
# it, and the attached TUI draws it.
#
# It needs `opencode`, `tmux` and `jq` on PATH. Anything missing is a failure:
# this script has no skip path. Run it from a plain terminal or CI, not from
# inside an agent: agentd treats a caller with a harness ancestor as that agent.
#
#   scripts/opencode-group-import-smoke/run.sh [workdir]
set -euo pipefail

ROOT=${1:-$(mktemp -d "${TMPDIR:-/tmp}/tclaude-oc-import.XXXXXX")}
mkdir -p "$ROOT"
ROOT=$(cd "$ROOT" && pwd)
BIN=$ROOT/bin
REPO=$(cd "$(dirname "$0")/../.." && pwd)
SECRET="heron-lantern-$RANDOM$RANDOM"
mkdir -p "$BIN"

log() { printf '\n== %s\n' "$*"; }
die() { echo "::error::$*" >&2; exit 1; }

for tool in opencode tmux jq; do
	command -v "$tool" >/dev/null || die "$tool is not installed"
done

log "building tclaude into $BIN"
(cd "$REPO" && go build -o "$BIN/tclaude" .)

# node <name> <cmd...>: run a command as that node's operator, in its own HOME,
# XDG directories and tmux server. Inherited TCLAUDE_* variables belong to the
# caller's own tclaude.
node() {
	local name=$1
	shift
	local unset=(-u TMUX -u XDG_RUNTIME_DIR)
	local v
	for v in $(compgen -e | grep '^TCLAUDE_' || true); do unset+=(-u "$v"); done
	env "${unset[@]}" HOME="$ROOT/$name" TMUX_TMPDIR="$ROOT/$name/tmux" \
		XDG_CONFIG_HOME="$ROOT/$name/.config" XDG_CACHE_HOME="$ROOT/$name/.cache" \
		XDG_DATA_HOME="$ROOT/$name/.local/share" XDG_STATE_HOME="$ROOT/$name/.local/state" \
		OPENCODE_DISABLE_AUTOUPDATE=1 PATH="$BIN:$PATH" "$@"
}

PIDS=()
cleanup() {
	local status=$?
	if ((status != 0)); then
		for name in src dst; do
			log "$name daemon log (tail)"
			tail -n 60 "$ROOT/$name/agentd.log" 2>/dev/null || true
			log "$name tmux panes"
			node "$name" tmux -L tclaude list-panes -a -F '#{session_name} #{pane_dead} #{pane_current_command}' 2>/dev/null || true
		done
	fi
	for p in "${PIDS[@]:-}"; do [[ -n $p ]] && kill "$p" 2>/dev/null || true; done
	for name in src dst; do node "$name" tmux -L tclaude kill-server 2>/dev/null || true; done
	wait 2>/dev/null || true
}
trap cleanup EXIT

wait_for() { # wait_for <seconds> <desc> <cmd...>
	local seconds=$1 desc=$2
	shift 2
	local deadline=$((SECONDS + seconds))
	while ((SECONDS < deadline)); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 0.5
	done
	die "timed out waiting for: $desc"
}

start_daemon() {
	local name=$1
	mkdir -p "$ROOT/$name/tmux"
	# The daemon's own cwd is not the landing directory, on purpose: nothing
	# here may depend on the two matching.
	(cd "$ROOT/$name" && node "$name" nohup tclaude agentd serve --no-tray \
		>"$ROOT/$name/agentd.log" 2>&1) &
	PIDS+=($!)
	wait_for 60 "$name agentd" node "$name" tclaude agent groups ls
}

SRC_CWD=$ROOT/src/project
DST_CWD=$ROOT/dst/landing
mkdir -p "$ROOT/src" "$ROOT/dst" "$SRC_CWD"
git -C "$SRC_CWD" init -q
git -C "$SRC_CWD" -c user.name=smoke -c user.email=smoke@example.invalid commit -q --allow-empty -m init

log "seeding a native session in the source node's OpenCode"
SOURCE_ID=ses_0123456789abfSmokeSession01
MESSAGE_ID=msg_0123456789abfSmokeMessage1
PART_ID=prt_0123456789abfSmokeMessPart
jq -n --arg id "$SOURCE_ID" --arg msg "$MESSAGE_ID" --arg part "$PART_ID" --arg cwd "$SRC_CWD" --arg secret "$SECRET" '{
	info: {id: $id, slug: "smoke", projectID: "global", directory: $cwd, title: "Smoke history",
		version: "1.18.6", time: {created: 1, updated: 1}},
	messages: [{
		info: {id: $msg, sessionID: $id, role: "user", time: {created: 1}, agent: "build",
			model: {providerID: "test", modelID: "fixture"}},
		parts: [{id: $part, sessionID: $id, messageID: $msg, type: "text", text: $secret}]
	}]
}' >"$ROOT/fixture.json"
(cd "$SRC_CWD" && node src opencode import "$ROOT/fixture.json" --pure --log-level ERROR)
node src opencode session list --format json --pure --log-level ERROR | tee "$ROOT/src-sessions.json"
jq -e --arg id "$SOURCE_ID" 'any(.[]; .id == $id)' "$ROOT/src-sessions.json" >/dev/null \
	|| die "OpenCode did not list the seeded session"

start_daemon src
log "exporting the group from the source node"
node src tclaude agent groups create crew
node src tclaude agent groups add crew "$SOURCE_ID" --role lead
node src tclaude agent groups export crew --format paf --out "$ROOT/crew.paf"
test -s "$ROOT/crew.paf" || die "the group export wrote nothing"

start_daemon dst
sessions_on_dst() {
	(cd "$DST_CWD" && node dst opencode session list --format json --pure --log-level ERROR)
}

log "dry run on the destination node"
mkdir -p "$DST_CWD"
git -C "$DST_CWD" init -q
git -C "$DST_CWD" -c user.name=smoke -c user.email=smoke@example.invalid commit -q --allow-empty -m init
node dst tclaude agent groups import "$ROOT/crew.paf" --dry-run --as crew-landed | tee "$ROOT/dry-run.txt"
grep -qE 'opencode, history native' "$ROOT/dry-run.txt" \
	|| die "the dry run does not report a native OpenCode landing"
if grep -qiE 'config only|cannot install' "$ROOT/dry-run.txt"; then
	die "the dry run still reports a configuration-only landing"
fi
[[ $(sessions_on_dst | jq -s 'add // [] | length') == 0 ]] || die "the dry run wrote to OpenCode's store"

log "importing on the destination node"
node dst tclaude agent groups import "$ROOT/crew.paf" --into "$DST_CWD" --as crew-landed --json \
	| tee "$ROOT/import.json"
jq -e '.landings | length == 1 and .[0].harness == "opencode" and .[0].rung == "native"' "$ROOT/import.json" >/dev/null \
	|| die "the import result does not report a native OpenCode landing"
LANDED=$(sessions_on_dst | jq -r '.[0].id')
[[ $LANDED == ses_* && $LANDED != "$SOURCE_ID" ]] || die "no minted session in OpenCode's store: $LANDED"
[[ $(sessions_on_dst | jq -r '.[0].directory') == "$DST_CWD" ]] || die "the session is not bound to the landing directory"

log "OpenCode exports the imported conversation"
(cd "$DST_CWD" && node dst opencode export "$LANDED" --pure --log-level ERROR) >"$ROOT/export.json"
grep -q "$SECRET" "$ROOT/export.json" || die "OpenCode's export does not hold the imported conversation"

log "resuming the group"
node dst tclaude agent groups resume crew-landed | tee "$ROOT/resume.txt"
grep -qE 'resumed|already_online' "$ROOT/resume.txt" || die "the member did not resume"

# The attached TUI needs a moment to fetch the session and draw it. Sized as an
# outer bound, not an expected delay.
pane_shows_history() {
	local session
	for session in $(node dst tmux -L tclaude list-sessions -F '#{session_name}' 2>/dev/null); do
		node dst tmux -L tclaude capture-pane -p -J -t "$session" 2>/dev/null \
			| tee "$ROOT/pane.txt" | grep -q "$SECRET" && return 0
	done
	return 1
}
log "the attached OpenCode TUI draws the conversation"
wait_for 90 "the imported conversation in the OpenCode TUI" pane_shows_history
cat "$ROOT/pane.txt"

log "the managed server serves the same session and is ready for a turn"
DB=$ROOT/dst/.tclaude/data/db.sqlite
python3 - "$DB" "$LANDED" "$SECRET" "$DST_CWD" <<'PY'
import base64, json, sqlite3, sys, urllib.parse, urllib.request

db, conv, secret, cwd = sys.argv[1:5]
row = sqlite3.connect(db).execute(
    "SELECT server_url, password FROM opencode_runtimes WHERE conv_id = ?", (conv,)
).fetchone()
assert row, f"no managed OpenCode runtime for {conv}"
url, password = row
auth = base64.b64encode(f"opencode:{password}".encode()).decode()
q = urllib.parse.urlencode({"directory": cwd})


def get(path):
    req = urllib.request.Request(f"{url}{path}?{q}", headers={"Authorization": f"Basic {auth}"})
    with urllib.request.urlopen(req, timeout=10) as res:
        return json.load(res)


session = get(f"/session/{conv}")
assert session["id"] == conv, session
messages = json.dumps(get(f"/session/{conv}/message"))
assert secret in messages, "the managed server does not hold the imported messages"
print("managed server holds", conv)
PY

log "PASS: the imported OpenCode conversation is in the store, the server and the TUI"
