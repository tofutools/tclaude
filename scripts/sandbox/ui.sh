#!/usr/bin/env bash
# The dashboard sandbox: the real dashboard and daemon API, built from this
# working tree, over seeded data that fills every page, under a throwaway HOME.
# Drive it with your own browser (Playwright, or headless Chrome) before you
# ship a dashboard change.
#
#   scripts/sandbox/ui.sh up [--fixture NAME] [--port N]   start it, print the URL
#   scripts/sandbox/ui.sh up --foreground [...]            the same, serving until stopped
#                                                         (run it as a background job of
#                                                         your shell to `tclaude agent show` it)
#   scripts/sandbox/ui.sh up --peer [...]                  also a live federation peer and hub
#   scripts/sandbox/ui.sh url | status                     the running sandbox
#   scripts/sandbox/ui.sh down                             stop it, delete its data
#   scripts/sandbox/ui.sh smoke [--peer] [NAME]            Playwright tests on a fresh sandbox
#   scripts/sandbox/ui.sh tour [--peer]                    the route tour (smoke tour)
#
# Fixtures: full (the default: every page and tab has data), or any docshots
# fixture (fleet, collab, steward, learn, page, collab-fleet, collab-quiet).
# --port binds 127.0.0.1 only; the default is a random free port.
#
# The server is TestSandboxServe in pkg/claude/agentd, run from a test binary
# with tmux simulated: it never touches ~/.tclaude, the real daemon or the
# real tmux server. The clock is live and the API is writable (the operator's
# login is implied), so tasks, drags, drawings and messages all work. Terminals
# show the simulated panes only: there is no real tmux to attach to.
#
# --peer adds a second daemon (a child process of the server, with its own
# throwaway HOME) and a tclaude-hub inside the server, links the two as trusted
# peers and seeds the peer, so the Fleet map, the peer's pages and the
# hub-backed Fleet pages show real peer data. It adds a few seconds to the start.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
state_root="$root/sandbox-out"
smoke_dir="$root/scripts/sandbox/smoke"

die() { echo "ui.sh: $*" >&2; exit 1; }

usage() {
  sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//' >&2
  exit 2
}

# start NAME FIXTURE PORT [PEER]: build and start a sandbox whose state lives
# in $state_root/NAME; prints nothing, leaves its URL in $state_root/NAME/url.
# PEER non-empty links a live federation peer (--peer).
start() {
  local name="$1" fixture="$2" port="$3" peer="${4:-}"
  local state="$state_root/$name"
  if [[ -f "$state/pid" ]] && kill -0 "$(cat "$state/pid")" 2>/dev/null; then
    die "sandbox '$name' is already up at $(cat "$state/url" 2>/dev/null) (ui.sh down first)"
  fi
  stop "$name" # a dead one's leftovers, its throwaway directory included
  mkdir -p "$state"
  local work
  work="$(mktemp -d "${TMPDIR:-/tmp}/tclaude-sandbox.XXXXXX")"
  echo "$work" > "$state/work"
  mkdir -p "$work"/{home,tmp,xdg/config,xdg/cache,xdg/data,xdg/state,xdg/runtime,serve}
  chmod 700 "$work/xdg/runtime"

  local t0=$SECONDS
  echo "building the sandbox server from $root ..." >&2
  (cd "$root" && go test -c -o "$work/agentd.test" ./pkg/claude/agentd/) || die "build failed"
  echo "built in $((SECONDS - t0))s; seeding fixture '$fixture'${peer:+ and a live peer} ..." >&2

  # The server sees only the throwaway directory: HOME, TMPDIR and every XDG
  # dir point inside it. It runs from the package directory, as go test does.
  # Detached (the default) it outlives this script: setsid where there is one
  # (stock macOS has none), else nohup. In the foreground it stays this
  # script's child, so it is in the caller's process tree (agent show checks
  # that) and stops with the script.
  local detach=()
  if [[ -z "$foreground" ]]; then
    detach=(nohup)
    command -v setsid >/dev/null && detach=(setsid)
  fi
  # The subshell execs into the server, so $! is the server's pid and, in the
  # foreground, its parent is this script.
  (
    cd "$root/pkg/claude/agentd" &&
    exec env HOME="$work/home" TMPDIR="$work/tmp" \
      XDG_CONFIG_HOME="$work/xdg/config" XDG_CACHE_HOME="$work/xdg/cache" \
      XDG_DATA_HOME="$work/xdg/data" XDG_STATE_HOME="$work/xdg/state" \
      XDG_RUNTIME_DIR="$work/xdg/runtime" \
      TCLAUDE_SANDBOX_SERVE="$work/serve" TCLAUDE_SANDBOX_FIXTURE="$fixture" \
      TCLAUDE_SANDBOX_PORT="$port" TCLAUDE_SANDBOX_PEER="$peer" \
      ${detach[@]+"${detach[@]}"} "$work/agentd.test" -test.run '^TestSandboxServe$' -test.timeout 0 -test.v
  ) > "$state/server.log" 2>&1 < /dev/null &
  echo $! > "$state/pid"
  local pid
  pid="$(cat "$state/pid")"
  local deadline=$((SECONDS + 120))
  until [[ -f "$work/serve/url" ]]; do
    if ! kill -0 "$pid" 2>/dev/null; then
      grep -v 'pre-migration backup' "$state/server.log" | tail -20 >&2
      die "the sandbox server exited (log: $state/server.log)"
    fi
    (( SECONDS < deadline )) || die "no URL after 120s (log: $state/server.log)"
    sleep 0.2
  done
  cp "$work/serve/url" "$state/url"
  [[ -f "$work/serve/fixture.json" ]] && cp "$work/serve/fixture.json" "$state/fixture.json"
  echo "up in $((SECONDS - t0))s" >&2
}

# stop NAME: stop the sandbox and delete its throwaway directory.
stop() {
  local state="$state_root/$1"
  [[ -d "$state" ]] || return 0
  local work pid
  work="$(cat "$state/work" 2>/dev/null || true)"
  pid="$(cat "$state/pid" 2>/dev/null || true)"
  if [[ -n "$work" && -d "$work/serve" ]]; then
    touch "$work/serve/stop"
  fi
  # The live peer (--peer) is the server's child: it stops when the server
  # closes its stdin, or dies.
  local peer_pid=""
  [[ -n "$work" && -f "$work/serve/peer.pid" ]] && peer_pid="$(cat "$work/serve/peer.pid")"
  local p
  for p in "$pid" "$peer_pid"; do
    [[ -n "$p" ]] || continue
    # The server returns on the stop file and runs its cleanup; give it time,
    # then TERM it and wait again, so its files are not deleted under it.
    for _ in $(seq 100); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
    if kill -0 "$p" 2>/dev/null; then
      kill "$p" 2>/dev/null || true
      for _ in $(seq 50); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
      kill -9 "$p" 2>/dev/null || true
    fi
  done
  # Keep the server's log: it says why a sandbox failed or what it refused.
  [[ -f "$state/server.log" ]] && cp "$state/server.log" "$state_root/$1-last.log"
  [[ -n "$work" && -f "$work/serve/peer.log" ]] && cp "$work/serve/peer.log" "$state_root/$1-peer-last.log"
  # Only ever delete the directory this script made.
  if [[ -n "$work" && "$work" == */tclaude-sandbox.* && -d "$work" ]]; then
    rm -rf "$work"
  fi
  rm -rf "$state"
}

# run_smoke NAME TEST...: Playwright against the sandbox NAME. Results and
# screenshots go to sandbox-out/results-NAME, kept after the run.
run_smoke() {
  local state="$state_root/$1"
  local results="$state_root/results-$1"; shift
  [[ -d "$smoke_dir/node_modules/@playwright/test" ]] ||
    (cd "$smoke_dir" && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm ci --no-audit --no-fund) >&2
  local work
  work="$(cat "$state/work")"
  local fixture_env=()
  [[ -f "$state/fixture.json" ]] && fixture_env=(SANDBOX_FIXTURE_JSON="$state/fixture.json")
  # Chrome writes under the XDG dirs, and ~/.config may not be writable under
  # an agent sandbox (crashpad then aborts), so they point at the throwaway dir.
  (
    cd "$smoke_dir"
    env XDG_CONFIG_HOME="$work/xdg/config" XDG_CACHE_HOME="$work/xdg/cache" XDG_DATA_HOME="$work/xdg/data" \
      SANDBOX_URL="$(cat "$state/url")" ${fixture_env[@]+"${fixture_env[@]}"} \
      SANDBOX_OUT="$results" \
      node node_modules/@playwright/test/cli.js test "$@"
  )
}

cmd="${1:-}"
[[ -n "$cmd" ]] || usage
shift
case "$cmd" in
  up)
    fixture=full port="" foreground="" peer=""
    while (( $# )); do
      case "$1" in
        --fixture) fixture="${2:?--fixture needs a name}"; shift ;;
        --port) port="${2:?--port needs a number}"; shift ;;
        --foreground) foreground=1 ;;
        --peer) peer=1 ;;
        *) usage ;;
      esac
      shift
    done
    [[ -z "$port" || "$port" =~ ^[0-9]+$ ]] || die "--port must be a number"
    start ui "$fixture" "$port" "$peer"
    cat "$state_root/ui/url"; echo
    if [[ -n "$foreground" ]]; then
      # Only now: a start that failed (one already up, say) must not stop it.
      trap 'stop ui' EXIT
      trap 'exit 130' INT
      trap 'exit 143' TERM
      # Serve until the server ends: ui.sh down, or this script is stopped.
      status=0
      wait "$(cat "$state_root/ui/pid")" || status=$?
      exit "$status"
    fi
    ;;
  url)
    [[ -f "$state_root/ui/url" ]] || die "no sandbox is up (ui.sh up)"
    cat "$state_root/ui/url"; echo
    ;;
  status)
    if [[ -f "$state_root/ui/pid" ]] && kill -0 "$(cat "$state_root/ui/pid")" 2>/dev/null; then
      peer_note=""
      [[ -f "$(cat "$state_root/ui/work")/serve/peer.pid" ]] && peer_note=", with a live peer"
      echo "up: $(cat "$state_root/ui/url") (pid $(cat "$state_root/ui/pid"), data in $(cat "$state_root/ui/work")$peer_note)"
    else
      echo "down"
    fi
    ;;
  down)
    stop ui
    ;;
  smoke|tour)
    peer=""
    if [[ "${1:-}" == --peer ]]; then peer=1; shift; fi
    [[ "$cmd" == tour ]] && { (( $# == 0 )) || usage; set -- tour; }
    name="smoke-$$" foreground=""
    trap 'stop "$name"' EXIT
    start "$name" full "" "$peer"
    run_smoke "$name" "$@"
    echo "results and screenshots: $state_root/results-$name" >&2
    ;;
  *) usage ;;
esac
