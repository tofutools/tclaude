#!/usr/bin/env bash
# Shared plumbing: logging, GitHub step-summary output, and hard failure.
#
# Everything here degrades to plain stdout when GITHUB_STEP_SUMMARY is unset,
# so the entrypoint behaves identically when a human runs it outside CI.

smoke::log() { printf '\n=== %s\n' "$*"; }

# smoke::summary writes an operator-facing block. CI renders it on the job
# page, which is the only place a failure explains itself to someone who was
# not watching the log scroll past.
smoke::summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    cat >> "$GITHUB_STEP_SUMMARY"
  else
    cat
  fi
}

# smoke::error emits a CI annotation and the same text on stderr.
smoke::error() {
  printf '::error::%s\n' "$*"
  printf 'ERROR: %s\n' "$*" >&2
}

# smoke::require_command fails early and by name. A smoke that dies because a
# tool is absent must not be mistakable for a boundary that refused something.
smoke::require_command() {
  local missing=0 tool
  for tool in "$@"; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      smoke::error "required command not found: $tool"
      missing=1
    fi
  done
  return "$missing"
}

# smoke::kill_listener PID... — stop a fixture listener started as
# `sudo ... socat ... &`. Needs `pkill` (procps), which the shards assert in
# their prerequisite list rather than discovering here.
#
# `$!` there is the PID of the SUDO WRAPPER. Whether that is also the listener
# depends on how sudo was built and invoked: when it needs no pty and no I/O
# log it may exec the command in place, but when it forks, killing the wrapper
# leaves the real socat holding the bind. The next flow then fails on an
# address:port that is still answering from the previous one — a failure that
# reads like a policy result and is not one.
#
# One level of children is what the callers need, and that rests on an
# assumption worth naming: the intermediates in front of socat exec in place
# rather than fork. `sudo` may fork (which is the whole defect), while
# `ip netns exec` execs — iproute2 forks only in batch mode. A forking
# intermediate would silently reinstate the original bug.
#
# So the wrapper's children are signalled and then the wrapper, and every step
# tolerates an already-dead process: cleanup runs on the failure path where
# some of this never started. One level of children is enough for the bind —
# a `socat ...,fork` per-connection child is a grandchild and holds no
# listening socket.
smoke::kill_listener() {
  local pid
  for pid in "$@"; do
    [[ -n "${pid:-}" ]] || continue
    sudo pkill -TERM -P "$pid" 2>/dev/null || true
    sudo kill -TERM "$pid" 2>/dev/null || true
  done
}

# smoke::trap_cleanup FUNC — install FUNC as the cleanup for EXIT *and* for
# INT/TERM.
#
# What EXIT alone actually costs is worth stating precisely, because the
# obvious answer is wrong: bash DOES run an EXIT trap when a fatal signal kills
# the shell, so the teardown itself is not lost. What is lost is the FAILURE. A
# script interrupted with SIGINT and no INT trap exits 0, and smoke::run_flows
# judges a flow by PIPESTATUS[0] — so a cancelled or Ctrl-C'd flow scores as a
# pass on the status axis, and only the evidence check stands between that and
# a green run. These handlers re-raise as an exit status, so an interrupted
# flow FAILS.
#
# GitHub sends SIGINT first when a job is cancelled, which is exactly the
# signal whose default leaves the status at 0.
#
# FUNC is attempted at most once per shell: the signal handlers exit, which
# fires the EXIT trap as well, and a fixture teardown running twice would
# report spurious errors from the second pass. "Attempted", not "completed" — a
# teardown interrupted partway by a second signal is not retried.
smoke::trap_cleanup() {
  local fn="$1"
  # The name is interpolated into three trap strings, so it is checked before
  # it is installed: this refuses anything that is not already a function,
  # which both keeps the interpolation structurally safe and surfaces a typo'd
  # cleanup name HERE rather than as "command not found" during teardown.
  if ! declare -F "$fn" >/dev/null; then
    smoke::error "smoke::trap_cleanup: $fn is not a function"
    return 1
  fi
  # shellcheck disable=SC2064  # fn must expand now: the trap outlives this call.
  trap "smoke::run_cleanup_once $fn" EXIT
  # shellcheck disable=SC2064
  trap "smoke::run_cleanup_once $fn; exit 130" INT
  # shellcheck disable=SC2064
  trap "smoke::run_cleanup_once $fn; exit 143" TERM
}

smoke::run_cleanup_once() {
  [[ -z "${SMOKE_CLEANUP_RAN:-}" ]] || return 0
  SMOKE_CLEANUP_RAN=1
  "$1"
}
