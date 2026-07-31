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
