#!/usr/bin/env bash
# Record the skynet demo clips and a titled reel from a fresh e2e environment.
# See "Recording demos" in docs/e2e-manual-verification.md.
#
#   scripts/e2e/demo/record.sh all          # setup + every clip + reel
#   scripts/e2e/demo/record.sh <step>...    # setup | pairing | map | terminal | move | boards | hub | files | reel
#
# Expects `e2e.sh build`, then `e2e.sh up` and `e2e.sh browser start` on a fresh
# E2E_BASE that has NOT been paired: the pairing clip pairs the nodes on camera.
# Steps run in the order given; each one needs the steps before it in `all`.
# A UI step that finds nothing fails the run instead of recording captions over
# nothing. Needs ffmpeg and asciinema 2.x (asciicast v2, which the pinned
# asciinema-player 3.8 plays).
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
E=$HERE/../e2e.sh
BASE=$(realpath -m "${E2E_BASE:-${TMPDIR:-/tmp}/tce2e}")
export E2E_BASE=$BASE
D=$(realpath -m "${E2E_BIN:-$BASE/bin}")/drive
OUT=${OUT:-$BASE/demos}
HTTP_PORT=18499
A=http://127.0.0.1:18481
B=http://127.0.0.1:18482
mkdir -p "$OUT"

# --- helpers ---------------------------------------------------------------

rec_start() {
	rm -rf "$OUT/$1.frames"
	DRIVE_SHOW=1 "$D" record "$OUT/$1.frames" "$OUT/$1.mp4" >"$OUT/$1.rec.log" 2>&1 &
	REC_PID=$!
	sleep 1
}
rec_stop() {
	sleep 1.5
	kill -INT "$REC_PID" 2>/dev/null || true
	wait "$REC_PID" || true
	REC_PID=
	tail -1 "$OUT/$1.rec.log"
}
cleanup() {
	if [[ -n ${REC_PID:-} ]]; then
		kill -INT "$REC_PID" 2>/dev/null || true
		wait "$REC_PID" 2>/dev/null || true
	fi
	[[ -z ${HTTP_PID:-} ]] || kill "$HTTP_PID" 2>/dev/null || true
}
trap cleanup EXIT
# checked <cmd...>: run a drive command; a NOTFOUND result fails the step.
checked() {
	local out
	out=$("$@")
	if [[ $out == *NOTFOUND* ]]; then
		echo "step failed: ${*:2}" >&2
		return 1
	fi
}
d() { DRIVE_SHOW=1 checked "$D" "$@"; }
cap() { "$D" caption "$*" >/dev/null; sleep "${CAP_HOLD:-2.2}"; }
nocap() { "$D" caption >/dev/null; }
js() { checked "$D" eval "$1"; }
js_click() { js "(()=>{const e=$1; if(!e) return 'NOTFOUND'; e.scrollIntoView({block:'center'}); e.click(); return 'ok'})()"; }
btn() { js_click "[...document.querySelectorAll('button')].find(b=>b.offsetParent&&b.innerText.trim()==='$1')"; }
label() { js_click "[...document.querySelectorAll('label')].find(l=>l.offsetParent&&l.innerText.trim()==='$1')"; }
# select_nth <index among visible selects> <value>: real input+change events, so the app state follows.
select_nth() { js "(()=>{const s=[...document.querySelectorAll('select')].filter(s=>s.offsetParent)[$1];s.value='$2';s.dispatchEvent(new Event('input',{bubbles:true}));s.dispatchEvent(new Event('change',{bubbles:true}))})()"; }
scroll_to() { js "(()=>{const e=[...document.querySelectorAll('*')].find(e=>e.offsetParent&&e.childElementCount===0&&e.textContent.trim()==='$1');if(e)window.scrollTo({top:e.getBoundingClientRect().top+scrollY-90,behavior:'smooth'})})()"; sleep 1.2; }
map_button() { js_click "[...document.querySelectorAll('button,a')].filter(e=>e.offsetParent&&e.getBoundingClientRect().x>1150&&e.getBoundingClientRect().y<100).pop()"; }
# drag <agent> <group key>: drag an agent row onto a group in "Groups · all nodes".
drag() { js "$(grep -v '^//' "$HERE/drag.js")('$1','$2')"; }
# disarm: an open terminal pane guards page unloads, which would stall `drive open`.
disarm() { "$D" eval "window.dispatchEvent(new CustomEvent('tclaude:auth-expired'))" >/dev/null; }
# groups <url>: "Groups · all nodes" with every group expanded.
groups() {
	"$D" open "$1/fleet" >/dev/null
	sleep 3
	js "(()=>{const g=[...document.querySelectorAll('details[data-group-key]')].filter(d=>d.offsetParent&&d.dataset.groupKey.includes('@'));if(!g.length)return 'NOTFOUND';g.forEach(d=>{d.open=true});return 'ok'})()"
	sleep 1
}
login() { "$D" open "$("$E" dash "$1")" >/dev/null; sleep 2; }
fleet() { "$D" open "$1/fleet-admin" >/dev/null; sleep 2; d clicktext "$2"; sleep 1.5; }
serve() {
	cp "$HERE/player.html" "$HERE/title.html" "$OUT/"
	if (exec 3<>/dev/tcp/127.0.0.1/"$HTTP_PORT") 2>/dev/null; then
		echo "port $HTTP_PORT is busy (a stale http.server?)" >&2
		return 1
	fi
	(cd "$OUT" && exec python3 -m http.server "$HTTP_PORT" --bind 127.0.0.1 >/dev/null 2>&1) &
	HTTP_PID=$!
	sleep 1
}
unserve() {
	kill "$HTTP_PID" 2>/dev/null || true
	HTTP_PID=
}

# cli_clip <name> <script>: record a scripted CLI session with asciinema, then
# play the cast in asciinema-player inside the same Chrome and screencast it.
cli_clip() {
	mkdir -p "$BASE/asciinema"
	ASCIINEMA_CONFIG_HOME=$BASE/asciinema asciinema rec -q --overwrite --cols 130 --rows 26 \
		-c "env E2E_BASE=$BASE $2" "$OUT/$1.cast" </dev/null
	serve
	"$D" open "http://127.0.0.1:$HTTP_PORT/player.html?cast=$1.cast" >/dev/null
	sleep 1
	local dur
	dur=$(python3 -c "import json;l=open('$OUT/$1.cast').read().splitlines();print(json.loads(l[-1])[0])")
	rec_start "$1"
	js '(window.player ? (window.player.play(), "ok") : "NOTFOUND")'
	sleep "$(python3 -c "print($dur+1.5)")"
	rec_stop "$1"
	unserve
}

# --- steps -----------------------------------------------------------------

step_pairing() { cli_clip 01-pairing-cli "$HERE/cli-pair.sh"; }

step_setup() { # groups and agents are created after pairing, as a user would
	"$E" run a agent groups create builders >/dev/null
	"$E" run a agent groups set-default-dir builders "$BASE/a/proj" >/dev/null
	"$E" run b agent groups create reviewers >/dev/null
	"$E" run b agent groups set-default-dir reviewers "$BASE/b/proj" >/dev/null
	"$E" run a agent spawn builders --name builder-1 -C "$BASE/a/proj" --initial-message 'Draft the 1.1 announcement' >/dev/null
	"$E" run b agent spawn reviewers --name reviewer-1 -C "$BASE/b/proj" --initial-message 'Review notes.txt for typos' >/dev/null
	"$E" run b agent spawn reviewers --name reviewer-2 -C "$BASE/b/proj" --initial-message 'Check the changelog' >/dev/null
	"$E" run b agent spawn reviewers --name reviewer-home -C "$BASE/b" --initial-message 'Rooted in HOME' >/dev/null
	printf 'Release 1.1 notes\n- federation boards\n- hub admin from the UI\n' >"$BASE/b/proj/notes.txt"
	echo 'API_TOKEN=not-a-real-secret' >"$BASE/b/proj/.env"
	printf '%s' '{"name":"security-reviewer","descr":"Security reviewer - audits diffs for auth and injection bugs","brief":"You are a security reviewer. Read each diff for auth, injection and secret-handling defects and report only real, exploitable issues.","permissions":[]}' >"$BASE/b/secrole.json"
	"$E" run b agent roles create --file "$BASE/b/secrole.json" >/dev/null
	login a
	login b
	sleep 15 # let the agents answer their first prompt
}

step_map() {
	"$D" open "$A/" >/dev/null
	sleep 2
	rec_start 02-map
	cap "Node a's dashboard: its own group, builders"
	CAP_HOLD=1 cap "Open the skynet map"
	map_button
	sleep 5
	cap "Both nodes, linked through the hub. b is an unrestricted peer (my own machine)"
	sleep 1.5
	cap "Groups · all nodes: every node's groups in one tree"
	d clicktext "Groups · all nodes"
	sleep 1.5
	for g in reviewers@b builders@node-a; do
		js_click "[...document.querySelectorAll('*')].find(e=>e.offsetParent&&e.childElementCount<3&&e.innerText&&e.innerText.trim()==='$g')"
		sleep 1.5
	done
	cap "b's reviewers sit next to a's builders, with live state"
	sleep 1.5
	nocap
	rec_stop 02-map
}

step_terminal() {
	"$D" open "$A/map" >/dev/null
	sleep 5
	rec_start 03-remote-terminal
	cap "From node a's map: open a terminal on node b"
	btn "Terminals…"
	sleep 1.5
	cap "b shares its agents. Open reviewer-2 interactively"
	js_click "[...document.querySelectorAll('tr,li,div')].filter(e=>e.offsetParent&&e.innerText.startsWith('reviewer-2')).map(e=>e.querySelector('button')).find(Boolean)"
	sleep 4
	cap "It opens in a's Terminals tab, next to local panes: b's live pane, streamed through the hub"
	CAP_HOLD=0.5 cap "Type into it from node a. b sees REMOTE INPUT a"
	d type '.xterm-helper-textarea' 'Hi from node a - can you check the changelog?'
	sleep 1.2
	d key Enter
	sleep 3.5
	cap "The agent on b answers in the same terminal (mock model, no credentials)"
	sleep 1.5
	nocap
	rec_stop 03-remote-terminal
	disarm
}

step_move() {
	# a restricts b and lets it offer agents into builders, with no landing
	# policy: b's agents can reach a only as offers a accepts. b keeps trusting
	# a unrestricted, so a's agents land on b at once.
	local idb
	idb=$("$E" run b federation identity 2>/dev/null | head -1)
	"$E" run a federation trust "$idb" --level restricted --yes --label b >/dev/null
	"$E" run a federation grant b agents.receive --scope group=builders >/dev/null
	groups "$A"
	rec_start 04-move-agent
	cap "Node a, Groups · all nodes: drag an agent onto a group on another node"
	CAP_HOLD=0.5 cap "Drag a's builder-1 onto b's reviewers"
	drag builder-1 reviewers@b
	sleep 1
	cap "Confirm where it goes. Its config and full history travel with it"
	d key Enter
	sleep 1
	cap "b trusts a fully, so b takes it in directly"
	d wait "landed in group" 60
	sleep 1
	cap "Landed on b, in reviewers. a retires its copy"
	sleep 1.5
	d click '#skynet-move-drop-close'
	sleep 2
	cap "a restricts b: b's agents may only be offered into builders, for a to accept"
	CAP_HOLD=0.5 cap "Drag b's reviewer-2 onto a's builders"
	drag reviewer-2 builders@node-a
	sleep 1
	d key Enter
	d wait "waiting for you to accept" 60
	sleep 1
	cap "No landing policy for b here, so it waits for a to accept"
	d click '[data-move-drop="moves"]'
	sleep 2
	cap "Fleet → Offers: the pending move, with why it waits"
	sleep 1
	d clicktext "Preview…"
	sleep 2
	cap "a decides where it lands: same path, the group's default dir, or any dir a owns"
	sleep 1
	select_nth 0 group_default
	sleep 2
	cap "Pick the builders group's default dir"
	d clicktext "Start agent"
	sleep 1.5
	cap "Accept. Once it runs here, b retires its source"
	d key Enter
	sleep 4
	d clicktext Moves
	sleep 2
	cap "Fleet → Moves: one move out to b, one move in from b"
	sleep 1.5
	nocap
	groups "$A"
	cap "reviewer-2 now runs in a's builders, builder-1 in b's reviewers"
	sleep 2
	nocap
	rec_stop 04-move-agent
	"$E" run a federation trust "$idb" --level unrestricted --yes --label b >/dev/null # as the map clip shows it
}

step_boards() {
	fleet "$B" Boards
	rec_start 05-boards
	cap "Boards: share config through the hub, without giving anyone access to your machine"
	cap "Node b creates a board"
	d type 'input[placeholder="New board name"]' 'team-presets'
	sleep 0.6
	d clicktext "Create" # a new board opens expanded
	sleep 2.5
	cap "Invite someone who can read, valid for 1 hour"
	d clicktext "Create invite"
	sleep 1
	d key Enter
	sleep 2
	local token
	token=$("$D" eval "(()=>{const e=[...document.querySelectorAll('*')].find(e=>e.childElementCount===0&&/^board1_/.test(e.textContent.trim()));return e?e.textContent.trim():''})()")
	cap "The invite is shown once. Hand it to node a out of band"
	sleep 1
	cap "b posts its roles to the board"
	d clicktext "Post config"
	sleep 1.2
	d type 'input[placeholder^="what members see"]' 'review roles'
	label roles
	sleep 1
	d clicktext "Post…"
	sleep 1.5
	cap "Posts are signed by b and end-to-end encrypted: the hub can't read them"
	d key Enter
	sleep 2.5
	nocap
	fleet "$A" Boards
	cap "Node a pastes the invite and joins"
	d type '#fleet-board-token' "$token"
	sleep 0.8
	btn Join
	sleep 3
	cap "a is a reader. b's post is there"
	sleep 0.5
	d clicktext "Open…"
	sleep 1.5
	cap "Inspect before importing: nothing changes on a until you import"
	d clicktext "sections/roles.json"
	sleep 2.5
	d clicktext "Import…"
	sleep 2
	cap "A per-item preview: security-reviewer is new here, the rest are unchanged"
	sleep 1
	d clicktext "Import…"
	sleep 1.2
	d key Enter
	sleep 2.5
	cap "Imported. a now has b's security-reviewer role"
	sleep 1
	nocap
	rec_stop 05-boards
}

step_hub() {
	fleet "$A" Hub
	rec_start 06-hub-admin
	cap "Administer the hub from tclaude: no shell on the hub machine after install"
	cap "The hub printed a one-time claim token on first start"
	d clicktext "Claim hub admin"
	sleep 1
	d type '#fleet-hub-claim-token' "$(cat "$BASE/hub/data/admin-claim.token")"
	sleep 0.8
	d key Enter
	sleep 1.2
	cap "Confirm: admin is bound to this node's key, and the token is consumed"
	d key Enter
	sleep 2.5
	cap "Node a is now hub admin: health, admissions, invites, admins"
	sleep 1.5
	scroll_to Invites
	cap "Issue node invites from here instead of the hub's shell"
	scroll_to Admins
	cap "Admins are instances; each holds explicit capabilities"
	scroll_to Settings
	cap "Hub settings in readable units: rates, limits and timeouts, each with its range"
	sleep 2
	scroll_to "Remote scripts"
	cap "Boards, remote scripts (off unless the hub host allows them) and an audit log"
	sleep 1
	scroll_to "Log tail"
	cap "Redacted log tail. The hub only relays ciphertext: admin never sees node content"
	sleep 1
	nocap
	rec_stop 06-hub-admin
}

step_files() { cli_clip 07-remote-files-cli "$HERE/cli-files.sh"; }

step_reel() {
	serve
	card() { # card <name> <kicker> <title> <subtitle> [seconds]
		local q
		q=$(python3 -c 'import sys,urllib.parse as u;print("k=%s&t=%s&s=%s"%tuple(u.quote(a) for a in sys.argv[1:]))' "$2" "$3" "$4")
		"$D" open "http://127.0.0.1:$HTTP_PORT/title.html?$q" >/dev/null
		sleep 0.5
		"$D" shot "$OUT/$1.png" >/dev/null
		ffmpeg -loglevel error -y -loop 1 -t "${5:-3}" -i "$OUT/$1.png" -vf "fps=25,scale=1280:800,format=yuvj420p" \
			-c:v libx264 -preset veryfast -crf 23 "$OUT/$1.mp4"
	}
	card t00 "tclaude skynet" "Two nodes, one hub" "A hub plus two isolated tclaude nodes on one machine, real Claude Code agents against a mock model. Recorded from the E2E runbook." 4
	card t01 "1 / 7 · CLI" "Pairing" "Hub invites, connect, compare fingerprints, trust"
	card t02 "2 / 7 · Dashboard" "The skynet map" "Both nodes and their groups in one view"
	card t03 "3 / 7 · Dashboard" "Remote terminal" "Type into an agent on another node, from the Terminals tab"
	card t04 "4 / 7 · Dashboard" "Move an agent" "Drag it to another node: it lands at once, or waits as an offer"
	card t05 "5 / 7 · Dashboard" "Boards" "Share config through the hub, without peer access"
	card t06 "6 / 7 · Dashboard" "Hub admin" "Claim and administer the hub from tclaude"
	card t07 "7 / 7 · CLI" "Remote files" "Read-only downloads, with guard rails"
	unserve
	{
		echo "file 't00.mp4'"
		for c in 01-pairing-cli 02-map 03-remote-terminal 04-move-agent 05-boards 06-hub-admin 07-remote-files-cli; do
			echo "file 't${c:0:2}.mp4'"
			echo "file '$c.mp4'"
		done
	} >"$OUT/reel.txt"
	(cd "$OUT" && ffmpeg -loglevel error -y -f concat -safe 0 -i reel.txt -c copy skynet-demo-reel.mp4)
	echo "wrote $OUT/skynet-demo-reel.mp4"
}

[[ $# -gt 0 ]] || set -- all
[[ $1 == all ]] && set -- pairing setup map terminal move boards hub files reel
for s in "$@"; do
	echo "== $s"
	"step_$s"
done
