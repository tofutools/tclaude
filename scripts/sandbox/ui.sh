#!/usr/bin/env bash
# The dashboard sandbox: the real dashboard and daemon API, built from this
# working tree, over seeded data that fills every page, under a throwaway HOME.
# Drive it with your own browser (Playwright, or headless Chrome) before you
# ship a dashboard change.
#
#   scripts/sandbox/ui.sh up [--fixture NAME] [--port N]   start it, print the URL
#   scripts/sandbox/ui.sh url | status                     the running sandbox
#   scripts/sandbox/ui.sh down                             stop it, delete its data
#   scripts/sandbox/ui.sh smoke [NAME]                     Playwright tests on a fresh sandbox
#   scripts/sandbox/ui.sh tour                             the route tour (smoke tour)
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
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
state_root="$root/sandbox-out"
smoke_dir="$root/scripts/sandbox/smoke"

die() { echo "ui.sh: $*" >&2; exit 1; }

usage() {
  sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d; s/^# \{0,1\}//' >&2
  exit 2
}

# start NAME FIXTURE PORT: build and start a sandbox whose state lives in
# $state_root/NAME; prints nothing, leaves its URL in $state_root/NAME/url.
start() {
  local name="$1" fixture="$2" port="$3"
  local state="$state_root/$name"
  if [[ -f "$state/pid" ]] && kill -0 "$(cat "$state/pid")" 2>/dev/null; then
    die "sandbox '$name' is already up at $(cat "$state/url" 2>/dev/null) (ui.sh down first)"
  fi
  rm -rf "$state"
  mkdir -p "$state"
  local work
  work="$(mktemp -d "${TMPDIR:-/tmp}/tclaude-sandbox.XXXXXX")"
  echo "$work" > "$state/work"
  mkdir -p "$work"/{home,tmp,xdg/config,xdg/cache,xdg/data,xdg/state,xdg/runtime,serve}
  chmod 700 "$work/xdg/runtime"

  local t0=$SECONDS
  echo "building the sandbox server from $root ..." >&2
  (cd "$root" && go test -c -o "$work/agentd.test" ./pkg/claude/agentd/) || die "build failed"
  echo "built in $((SECONDS - t0))s; seeding fixture '$fixture' ..." >&2

  # The server sees only the throwaway directory: HOME, TMPDIR and every XDG
  # dir point inside it. It runs from the package directory, as go test does.
  (
    cd "$root/pkg/claude/agentd"
    env HOME="$work/home" TMPDIR="$work/tmp" \
      XDG_CONFIG_HOME="$work/xdg/config" XDG_CACHE_HOME="$work/xdg/cache" \
      XDG_DATA_HOME="$work/xdg/data" XDG_STATE_HOME="$work/xdg/state" \
      XDG_RUNTIME_DIR="$work/xdg/runtime" \
      TCLAUDE_SANDBOX_SERVE="$work/serve" TCLAUDE_SANDBOX_FIXTURE="$fixture" \
      TCLAUDE_SANDBOX_PORT="$port" \
      setsid "$work/agentd.test" -test.run '^TestSandboxServe$' -test.timeout 0 -test.v \
      > "$state/server.log" 2>&1 < /dev/null &
    echo $! > "$state/pid"
  )
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
  if [[ -n "$pid" ]]; then
    for _ in $(seq 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    kill "$pid" 2>/dev/null || true
  fi
  # Only ever delete the directory this script made.
  if [[ -n "$work" && "$work" == */tclaude-sandbox.* && -d "$work" ]]; then
    rm -rf "$work"
  fi
  rm -rf "$state"
}

# smoke NAME TEST...: Playwright against the sandbox NAME.
run_smoke() {
  local state="$state_root/$1"; shift
  [[ -d "$smoke_dir/node_modules/@playwright/test" ]] ||
    (cd "$smoke_dir" && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm ci --no-audit --no-fund) >&2
  local work
  work="$(cat "$state/work")"
  # Chrome writes under the XDG dirs, and ~/.config may not be writable under
  # an agent sandbox (crashpad then aborts), so they point at the throwaway dir.
  (
    cd "$smoke_dir"
    env XDG_CONFIG_HOME="$work/xdg/config" XDG_CACHE_HOME="$work/xdg/cache" XDG_DATA_HOME="$work/xdg/data" \
      SANDBOX_URL="$(cat "$state/url")" SANDBOX_FIXTURE_JSON="$state/fixture.json" \
      SANDBOX_OUT="$root/sandbox-out/results" \
      node node_modules/@playwright/test/cli.js test "$@"
  )
}

cmd="${1:-}"
[[ -n "$cmd" ]] || usage
shift
case "$cmd" in
  up)
    fixture=full port=""
    while (( $# )); do
      case "$1" in
        --fixture) fixture="${2:?--fixture needs a name}"; shift ;;
        --port) port="${2:?--port needs a number}"; shift ;;
        *) usage ;;
      esac
      shift
    done
    [[ -z "$port" || "$port" =~ ^[0-9]+$ ]] || die "--port must be a number"
    start ui "$fixture" "$port"
    cat "$state_root/ui/url"; echo
    ;;
  url)
    [[ -f "$state_root/ui/url" ]] || die "no sandbox is up (ui.sh up)"
    cat "$state_root/ui/url"; echo
    ;;
  status)
    if [[ -f "$state_root/ui/pid" ]] && kill -0 "$(cat "$state_root/ui/pid")" 2>/dev/null; then
      echo "up: $(cat "$state_root/ui/url") (pid $(cat "$state_root/ui/pid"), data in $(cat "$state_root/ui/work"))"
    else
      echo "down"
    fi
    ;;
  down)
    stop ui
    ;;
  smoke|tour)
    [[ "$cmd" == tour ]] && set -- tour
    name="smoke-$$"
    trap 'stop "$name"' EXIT
    start "$name" full ""
    run_smoke "$name" "$@"
    ;;
  *) usage ;;
esac
