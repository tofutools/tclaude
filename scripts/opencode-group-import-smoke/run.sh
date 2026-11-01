#!/usr/bin/env bash
# OpenCode group import smoke: a real OpenCode, two isolated tclaude nodes, and
# no model call.
#
# Node "src" imports a native session into OpenCode's own store, puts it in a
# group and exports the group. Node "dst", with an empty OpenCode store, dry-runs
# and imports that archive, then resumes the group. The run passes when the
# conversation is in dst's store, OpenCode exports it, the managed server serves
# it, and the attached TUI draws it. The node then makes tclaude's sandbox its
# install default and imports the same archive again: that member's conversation
# goes in its private state, never the shared store, and resumes in a sandboxed
# server, and resume recovery still finds that conversation with its resume
# profile gone. Assigning tclaude's sandbox to the first member then moves its
# conversation into private state with the same ids, and an agent PAF of the
# same conversation lands offline both ways (`paf import --no-start`), with no
# server until it resumes. Assigning another implementation moves the first
# member back to the shared store and removes its private state.
#
# It needs `opencode`, `tmux`, `jq` and a working `bwrap` on PATH. Anything missing
# is a failure:
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

for tool in opencode tmux jq bwrap; do
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

start_daemon() { # start_daemon <name> <daemon cwd>
	local name=$1 cwd=$2
	mkdir -p "$ROOT/$name/tmux"
	(cd "$cwd" && node "$name" nohup tclaude agentd serve --no-tray \
		--persist-operator-token --no-print-human-token >"$ROOT/$name/agentd.log" 2>&1) &
	PIDS+=($!)
	wait_for 60 "$name agentd socket" test -S "$ROOT/$name/.tclaude/api/agentd-socket/agentd.sock"
	wait_for 60 "$name operator token" test -s "$ROOT/$name/.tclaude/data/operator_token"
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
NOW_MS=$(($(date +%s) * 1000))
jq -n --arg id "$SOURCE_ID" --arg msg "$MESSAGE_ID" --arg part "$PART_ID" --arg cwd "$SRC_CWD" --arg secret "$SECRET" \
	--argjson now "$NOW_MS" '{
	info: {id: $id, slug: "smoke", projectID: "global", directory: $cwd, title: "Smoke history",
		version: "1.18.6", time: {created: $now, updated: $now}},
	messages: [{
		info: {id: $msg, sessionID: $id, role: "user", time: {created: $now}, agent: "build",
			model: {providerID: "test", modelID: "fixture"}},
		parts: [{id: $part, sessionID: $id, messageID: $msg, type: "text", text: $secret}]
	}]
}' >"$ROOT/fixture.json"
(cd "$SRC_CWD" && node src opencode import "$ROOT/fixture.json" --pure --log-level ERROR)
# `opencode session list` shows only the project of its working directory.
(cd "$SRC_CWD" && node src opencode session list --format json --pure --log-level ERROR) | tee "$ROOT/src-sessions.json"
jq -e --arg id "$SOURCE_ID" 'any(.[]; .id == $id)' "$ROOT/src-sessions.json" >/dev/null \
	|| die "OpenCode did not list the seeded session"

# The source daemon runs in the session's project so it can see the fixture.
start_daemon src "$SRC_CWD"
log "exporting the group from the source node"
node src tclaude agent groups create crew
# Listing from the project indexes the session; nothing else has told tclaude
# about a conversation that no agent started.
(cd "$SRC_CWD" && node src tclaude conv ls)
node src tclaude agent groups add crew "$SOURCE_ID" --role lead
node src tclaude agent groups export crew --format paf --out "$ROOT/crew.paf"
test -s "$ROOT/crew.paf" || die "the group export wrote nothing"
node src tclaude paf export "$SOURCE_ID" --history --file "$ROOT/agent.paf"
test -s "$ROOT/agent.paf" || die "the agent export wrote nothing"

# The destination daemon does not run in the landing directory, as a real one
# would not, so its own `opencode session list` cannot see the landed member.
start_daemon dst "$ROOT/dst"
sessions_on_dst() {
	(cd "$DST_CWD" && node dst opencode session list --format json --pure --log-level ERROR)
}

log "dry run on the destination node"
mkdir -p "$DST_CWD"
git -C "$DST_CWD" init -q
git -C "$DST_CWD" -c user.name=smoke -c user.email=smoke@example.invalid commit -q --allow-empty -m init
node dst tclaude agent groups import "$ROOT/crew.paf" --dry-run --into "$DST_CWD" --as crew-landed | tee "$ROOT/dry-run.txt"
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

# managed_server_holds <conv> <expected sandbox implementation>: the conversation's
# managed server runs under that implementation and serves the messages.
managed_server_holds() {
	python3 - "$ROOT/dst/.tclaude/data/db.sqlite" "$1" "$SECRET" "$DST_CWD" "$2" <<'PY'
import base64, http.client, json, socket, sqlite3, sys, urllib.parse

db, conv, secret, cwd, implementation = sys.argv[1:6]
row = sqlite3.connect(db).execute(
    "SELECT server_url, password, sandbox_implementation, sandbox_launch_spec_json, control_socket_path"
    " FROM opencode_runtimes WHERE conv_id = ?", (conv,)
).fetchone()
assert row, f"no managed OpenCode runtime for {conv}"
url, password, recorded, spec, control = row
assert recorded == implementation, f"the server runs under {recorded}, not {implementation}"
assert bool(spec) == (implementation == "tclaude-layer"), "the sandbox launch spec does not match the implementation"
auth = base64.b64encode(f"opencode:{password}".encode()).decode()
q = urllib.parse.urlencode({"directory": cwd})


class UnixConnection(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(control)


def get(path):
    parsed = urllib.parse.urlparse(url)
    if control:
        conn = UnixConnection("localhost", timeout=10)
    else:
        conn = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=10)
    conn.request("GET", f"{path}?{q}", headers={"Authorization": f"Basic {auth}"})
    res = conn.getresponse()
    assert res.status == 200, f"{path}: HTTP {res.status}"
    return json.load(res)


session = get(f"/session/{conv}")
assert session["id"] == conv, session
messages = json.dumps(get(f"/session/{conv}/message"))
assert secret in messages, "the managed server does not hold the imported messages"
print("managed server holds", conv)
PY
}

log "the managed server serves the same session"
managed_server_holds "$LANDED" harness-builtin

# land_offline <group> <name> <expected sandbox implementation>: the agent lands
# with no server, and resume serves its conversation under that implementation.
OFFLINE=
land_offline() {
	node dst tclaude paf import --file "$ROOT/agent.paf" --cwd "$DST_CWD" --group "$1" --name "$2" \
		--apply --no-start --json | tee "$ROOT/offline-$2.json"
	OFFLINE=$(jq -r '.spawn.conv_id' "$ROOT/offline-$2.json")
	[[ $OFFLINE == ses_* ]] || die "the offline import returned no conversation: $OFFLINE"
	local running
	running=$(python3 - "$ROOT/dst/.tclaude/data/db.sqlite" "$OFFLINE" <<'PY'
import sqlite3, sys

print(sqlite3.connect(sys.argv[1]).execute(
    "SELECT count(*) FROM opencode_runtimes WHERE conv_id = ?", (sys.argv[2],)).fetchone()[0])
PY
)
	[[ $running == 0 ]] || die "an offline landing started a server"
	node dst tclaude agent resume "$OFFLINE" | tee "$ROOT/offline-$2-resume.txt"
	wait_for 90 "the offline agent's server" managed_server_holds "$OFFLINE" "$3"
}

log "landing an agent offline on the shared store"
land_offline crew-landed parked harness-builtin

log "importing again under tclaude's sandbox"
node dst tclaude agent groups stop crew-landed --force
node dst tmux -L tclaude kill-server 2>/dev/null || true
node dst tclaude agent sandbox-impl default set tclaude-layer
node dst tclaude agent stop "$OFFLINE" --force
AMBIENT_BEFORE=$(sessions_on_dst | jq -c '[.[].id] | sort')
node dst tclaude agent groups import "$ROOT/crew.paf" --dry-run --into "$DST_CWD" --as crew-walled | tee "$ROOT/walled-dry-run.txt"
grep -q 'sandbox tclaude-layer' "$ROOT/walled-dry-run.txt" || die "the dry run does not state the sandboxed posture"
grep -q 'private state' "$ROOT/walled-dry-run.txt" || die "the dry run does not state private OpenCode state"
node dst tclaude agent groups import "$ROOT/crew.paf" --into "$DST_CWD" --as crew-walled --json \
	| tee "$ROOT/walled-import.json"
jq -e '.landings[0] | .rung == "native" and .posture.sandbox_implementation == "tclaude-layer" and .posture.opencode_state == "private"' \
	"$ROOT/walled-import.json" >/dev/null || die "the import result does not state the sandboxed posture"
[[ $(sessions_on_dst | jq -c '[.[].id] | sort') == "$AMBIENT_BEFORE" ]] || die "the sandboxed member reached the shared store"

STATE_ROOT=$(python3 - "$ROOT/dst/.tclaude/data/db.sqlite" <<'PY'
import sqlite3, sys

rows = sqlite3.connect(sys.argv[1]).execute(
    "SELECT state_root FROM opencode_agent_state_allocations WHERE mode = 'private'"
).fetchall()
assert len(rows) == 1, rows
print(rows[0][0])
PY
)
[[ -d $STATE_ROOT ]] || die "no private state root: $STATE_ROOT"
private_opencode() {
	(cd "$DST_CWD" && node dst env XDG_DATA_HOME="$STATE_ROOT/data" XDG_CACHE_HOME="$STATE_ROOT/cache" \
		XDG_CONFIG_HOME="$STATE_ROOT/config" XDG_STATE_HOME="$STATE_ROOT/state" opencode "$@" --pure --log-level ERROR)
}
private_opencode session list --format json | tee "$ROOT/private-sessions.json"
WALLED=$(jq -r '.[0].id' "$ROOT/private-sessions.json")
[[ $WALLED == ses_* ]] || die "no session in private state: $WALLED"
[[ $WALLED != "$LANDED" ]] || die "the sandboxed member reused the shared store's session"
private_opencode export "$WALLED" >"$ROOT/private-export.json"
grep -q "$SECRET" "$ROOT/private-export.json" || die "private state does not hold the imported conversation"

log "resuming the sandboxed group"
node dst tclaude agent groups resume crew-walled | tee "$ROOT/walled-resume.txt"
grep -qE 'resumed|already_online' "$ROOT/walled-resume.txt" || die "the sandboxed member did not resume"
wait_for 90 "the sandboxed conversation in the OpenCode TUI" pane_shows_history
cat "$ROOT/pane.txt"
managed_server_holds "$WALLED" tclaude-layer

log "resume recovery finds a conversation in private state"
node dst tclaude agent stop "$WALLED" --force
# The managed server outlives the pane; a new one has to prove the resume.
node dst tmux -L tclaude kill-server 2>/dev/null || true

log "tclaude's own OpenCode runs load nothing an agent planted"
MARKERS=$ROOT/markers
AGENT_CONFIG=$STATE_ROOT/config/opencode
mkdir -p "$MARKERS" "$AGENT_CONFIG/plugin" "$DST_CWD/.opencode/plugin"
plant() { # plant <plugin file> <marker name>
	cat >"$1" <<JS
import fs from "node:fs"
fs.writeFileSync("$MARKERS/$2", "ran")
export const Planted = async () => ({})
JS
}
plant "$AGENT_CONFIG/plugin/planted.js" config-dir
plant "$DST_CWD/.opencode/plugin/planted.js" project-dir
plant "$ROOT/entry-config.js" config-entry
plant "$ROOT/entry-project.js" project-entry
[[ ! -e $AGENT_CONFIG/opencode.json ]] || cp "$AGENT_CONFIG/opencode.json" "$ROOT/agent-opencode.json"
jq -n --arg entry "file://$ROOT/entry-config.js" '{plugin: [$entry]}' >"$ROOT/planted-config.json"
if [[ -e $ROOT/agent-opencode.json ]]; then
	jq -s '.[0] + {plugin: ((.[0].plugin // []) + .[1].plugin)}' "$ROOT/agent-opencode.json" "$ROOT/planted-config.json" \
		>"$AGENT_CONFIG/opencode.json"
else
	cp "$ROOT/planted-config.json" "$AGENT_CONFIG/opencode.json"
fi
jq -n --arg entry "file://$ROOT/entry-project.js" '{plugin: [$entry]}' >"$DST_CWD/opencode.json"

# A wellknown auth entry in the data directory is remote config OpenCode fetches,
# so a listener counts those fetches.
python3 -u - "$ROOT/wellknown-hits" "$ROOT/wellknown-port" <<'PY' &
import http.server, sys


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(sys.argv[1], "a") as hits:
            hits.write(self.path + "\n")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"config":{}}')

    def log_message(self, *args):
        pass


server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
with open(sys.argv[2], "w") as port:
    port.write(str(server.server_port))
server.serve_forever()
PY
LISTENER=$!
PIDS+=("$LISTENER")
wait_for 10 "the wellknown listener" test -s "$ROOT/wellknown-port"
AGENT_AUTH=$STATE_ROOT/data/opencode/auth.json
[[ ! -e $AGENT_AUTH ]] || cp "$AGENT_AUTH" "$ROOT/agent-auth.json"
jq -n --arg url "http://127.0.0.1:$(cat "$ROOT/wellknown-port")" '{($url): {type: "wellknown", key: "K", token: "t"}}' >"$AGENT_AUTH"

# Without the hardening every one of these runs, or the check below proves nothing.
(cd "$DST_CWD" && node dst env XDG_DATA_HOME="$STATE_ROOT/data" XDG_CACHE_HOME="$STATE_ROOT/cache" \
	XDG_CONFIG_HOME="$STATE_ROOT/config" XDG_STATE_HOME="$STATE_ROOT/state" \
	opencode session list --format json --log-level ERROR) >/dev/null
for marker in config-dir project-dir config-entry project-entry; do
	[[ -e $MARKERS/$marker ]] || die "the planted $marker plugin did not run without the hardening"
done
# --pure stops the plugins and not this, so its control is a run with --pure.
private_opencode session list --format json >/dev/null
[[ -s $ROOT/wellknown-hits ]] || die "the planted auth entry was not fetched without the hardening"
rm -f "$MARKERS"/* "$ROOT/wellknown-hits"

# The private lookup, with no index row left to answer it; the ambient listing
# from the project; and a history export of the private conversation.
python3 - "$ROOT/dst/.tclaude/data/db.sqlite" "$WALLED" <<'PY'
import sqlite3, sys

with sqlite3.connect(sys.argv[1]) as db:
    n = db.execute("DELETE FROM conv_index WHERE conv_id = ?", (sys.argv[2],)).rowcount
assert n == 1, n
PY
(cd "$DST_CWD" && node dst tclaude conv archive "$WALLED")
python3 - "$ROOT/dst/.tclaude/data/db.sqlite" "$WALLED" <<'PY'
import sqlite3, sys

row = sqlite3.connect(sys.argv[1]).execute("SELECT 1 FROM conv_index WHERE conv_id = ?", (sys.argv[2],)).fetchone()
assert row, "the private lookup did not find the conversation"
PY
(cd "$DST_CWD" && node dst tclaude conv unarchive "$WALLED")
(cd "$DST_CWD" && node dst tclaude conv ls) >/dev/null
node dst tclaude paf export "$WALLED" --history --file "$ROOT/hardened.paf"
test -s "$ROOT/hardened.paf" || die "the history export wrote nothing"
[[ -z $(ls -A "$MARKERS") ]] || die "a planted plugin ran in tclaude's own run: $(ls "$MARKERS")"
[[ ! -e $ROOT/wellknown-hits ]] || die "a planted auth entry was fetched in tclaude's own run"

kill "$LISTENER" 2>/dev/null || true
if [[ -e $ROOT/agent-auth.json ]]; then
	cp "$ROOT/agent-auth.json" "$AGENT_AUTH"
else
	rm -f "$AGENT_AUTH"
fi
rm -rf "$AGENT_CONFIG/plugin" "$DST_CWD/.opencode" "$DST_CWD/opencode.json"
if [[ -e $ROOT/agent-opencode.json ]]; then
	cp "$ROOT/agent-opencode.json" "$AGENT_CONFIG/opencode.json"
else
	rm -f "$AGENT_CONFIG/opencode.json"
fi

python3 - "$ROOT/dst/.tclaude/data/db.sqlite" "$WALLED" <<'PY'
import sqlite3, sys

with sqlite3.connect(sys.argv[1]) as db:
    n = db.execute("DELETE FROM conversation_resume_profiles WHERE conv_id = ?", (sys.argv[2],)).rowcount
assert n == 1, n
PY
node dst tclaude agent resume "$WALLED" | tee "$ROOT/recovered-resume.txt"
grep -q 'resumed' "$ROOT/recovered-resume.txt" || die "the recovered conversation did not resume"
wait_for 90 "the recovered conversation's server" managed_server_holds "$WALLED" tclaude-layer
private_opencode session list --format json | jq -e --arg id "$WALLED" 'any(.[]; .id == $id)' >/dev/null \
	|| die "the recovered conversation left private state"

log "assigning tclaude's sandbox moves a shared-store agent into private state"
CHILD_ID=ses_0123456789abfSmokeChild0001
jq --arg id "$CHILD_ID" --arg parent "$LANDED" --arg cwd "$DST_CWD" '
	.info.id = $id | .info.parentID = $parent | .info.directory = $cwd |
	.messages[0].info.id = "msg_0123456789abfSmokeChildMsg" | .messages[0].info.sessionID = $id |
	.messages[0].parts[0].id = "prt_0123456789abfSmokeChildPrt" | .messages[0].parts[0].sessionID = $id |
	.messages[0].parts[0].messageID = "msg_0123456789abfSmokeChildMsg"' "$ROOT/fixture.json" >"$ROOT/child.json"
(cd "$DST_CWD" && node dst opencode import "$ROOT/child.json" --pure --log-level ERROR)
(cd "$DST_CWD" && node dst opencode export "$LANDED" --pure --log-level ERROR) >"$ROOT/before-move.json"
node dst tclaude agent groups stop crew-walled --force
node dst tmux -L tclaude kill-server 2>/dev/null || true
node dst tclaude agent sandbox-impl set "$LANDED" tclaude-layer | tee "$ROOT/move.txt"
grep -q 'todo list' "$ROOT/move.txt" || die "the assignment does not disclose the todo list"
sessions_on_dst | jq -es --arg id "$LANDED" 'add // [] | all(.[]; .id != $id)' >/dev/null || die "the shared copy is still there"
if (cd "$DST_CWD" && node dst opencode export "$CHILD_ID" --pure --log-level ERROR >/dev/null 2>&1); then
	die "the shared copy of the child session is still there"
fi
MOVED_ROOT=$(python3 - "$ROOT/dst/.tclaude/data/db.sqlite" "$STATE_ROOT" <<'PY'
import sqlite3, sys

rows = sqlite3.connect(sys.argv[1]).execute(
    "SELECT state_root FROM opencode_agent_state_allocations WHERE mode = 'private' AND state_root != ?",
    (sys.argv[2],),
).fetchall()
assert len(rows) == 1, rows
print(rows[0][0])
PY
)
STATE_ROOT=$MOVED_ROOT
private_opencode export "$LANDED" >"$ROOT/after-move.json"
cmp "$ROOT/before-move.json" "$ROOT/after-move.json" || die "the moved conversation changed"
private_opencode export "$CHILD_ID" | grep -q "$SECRET" || die "the child session did not move"
node dst tclaude agent resume "$LANDED" | tee "$ROOT/moved-resume.txt"
grep -q 'resumed' "$ROOT/moved-resume.txt" || die "the moved agent did not resume"
managed_server_holds "$LANDED" tclaude-layer

log "landing an agent offline under tclaude's sandbox"
# The move took the first member out of the shared store since the last snapshot.
AMBIENT_BEFORE=$(sessions_on_dst | jq -cs 'add // [] | [.[].id] | sort')
land_offline crew-walled parked-walled tclaude-layer
[[ $(sessions_on_dst | jq -cs 'add // [] | [.[].id] | sort') == "$AMBIENT_BEFORE" ]] || die "the offline sandboxed agent reached the shared store"

log "assigning another implementation moves the agent back to the shared store"
node dst tclaude agent groups stop crew-walled --force
node dst tmux -L tclaude kill-server 2>/dev/null || true
private_opencode export "$LANDED" >"$ROOT/before-move-out.json"
if node dst tclaude agent sandbox-impl set "$LANDED" harness-builtin >"$ROOT/move-out-refused.txt" 2>&1; then
	die "the move out did not ask first"
fi
grep -q "Run again with --yes" "$ROOT/move-out-refused.txt" || die "the refused move out does not say how to accept it"
[[ -e $MOVED_ROOT ]] || die "the refused move out touched the private state"
node dst tclaude agent sandbox-impl set "$LANDED" harness-builtin --yes | tee "$ROOT/move-out.txt"
grep -q "outside tclaude's sandbox" "$ROOT/move-out.txt" || die "the assignment does not say the conversation is readable"
(cd "$DST_CWD" && node dst opencode export "$LANDED" --pure --log-level ERROR) >"$ROOT/after-move-out.json"
cmp "$ROOT/before-move-out.json" "$ROOT/after-move-out.json" || die "the conversation changed moving out"
(cd "$DST_CWD" && node dst opencode export "$CHILD_ID" --pure --log-level ERROR) | grep -q "$SECRET" \
	|| die "the child session did not move out"
[[ ! -e $MOVED_ROOT ]] || die "the private state is still there"
node dst tclaude agent resume "$LANDED" | tee "$ROOT/moved-out-resume.txt"
grep -q 'resumed' "$ROOT/moved-out-resume.txt" || die "the moved-out agent did not resume"
managed_server_holds "$LANDED" harness-builtin

log "PASS: the imported OpenCode conversation is in the store, the server and the TUI, shared and sandboxed, started and offline"
