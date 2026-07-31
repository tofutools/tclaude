#!/usr/bin/env bash
# Pinned harness provisioning.
#
# The PINS LIVE HERE, not in a workflow file. That is the whole point of this
# layout: bumping Claude Code or Codex to a new pin is a repo edit an agent can
# make and a reviewer can read, not a change to .github/workflows that needs an
# operator with workflow scope. The smokes are evidence ABOUT these exact
# versions, so the version and the evidence belong in the same reviewable file.
#
# Every install ends by asserting the version it got. A silently-newer harness
# would quietly invalidate every cell the smokes back.

# Keep in step with the pins the Go smokes assert:
#   pkg/claude/session/filtered_network_model_endpoint_smoke_test.go
HARNESS_CLAUDE_VERSION="2.1.220"
HARNESS_CODEX_VERSION="0.145.0"
# Kept in step with the OpenCode pin the executor-smoke jobs in
# .github/workflows/ci.yml provision, so the carriage answer this shard records
# is about the same binary those smokes exercise. A carriage result is a fact
# about a version, not about OpenCode forever.
HARNESS_OPENCODE_VERSION="1.18.6"

harnesses::install_codex() {
  smoke::log "Installing pinned Codex ${HARNESS_CODEX_VERSION}"
  npm install --global "@openai/codex@${HARNESS_CODEX_VERSION}"
  # Must resolve under the read-only OS surface the constructed root binds,
  # for the same reason Claude is installed under /usr/local.
  local resolved
  resolved="$(command -v codex)"
  case "$resolved" in
    /usr/*|/opt/*) ;;
    *)
      smoke::error "codex resolved to $resolved, which the sandbox root does not bind"
      return 1
      ;;
  esac
  codex --version
  codex --version | grep -qF "$HARNESS_CODEX_VERSION" || {
    smoke::error "codex is not at the pinned version ${HARNESS_CODEX_VERSION}"
    return 1
  }
}

harnesses::install_claude() {
  smoke::log "Installing pinned Claude Code ${HARNESS_CLAUDE_VERSION}"
  local platform=linux-x64
  local base=https://downloads.claude.ai/claude-code-releases
  local tmp="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
  local manifest="$tmp/claude-manifest.json"
  local binary="$tmp/claude-${HARNESS_CLAUDE_VERSION}"
  local curl_args=(
    --fail --location --silent --show-error
    --max-time 60 --retry 2 --retry-all-errors
  )

  curl "${curl_args[@]}" --output "$manifest" \
    "$base/${HARNESS_CLAUDE_VERSION}/manifest.json"
  # The published checksum is verified before the binary is installed: this
  # runs a downloaded executable against real network policy, so provenance is
  # not optional.
  local checksum
  checksum=$(node -e '
    const fs = require("fs");
    const manifest = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    process.stdout.write(manifest.platforms[process.argv[2]]?.checksum ?? "");
  ' "$manifest" "$platform")
  if [[ ! "$checksum" =~ ^[a-f0-9]{64}$ ]]; then
    smoke::error "missing Claude Code checksum for $platform"
    return 1
  fi
  curl "${curl_args[@]}" --output "$binary" \
    "$base/${HARNESS_CLAUDE_VERSION}/$platform/claude"
  echo "$checksum  $binary" | sha256sum --check

  # Installed under /usr/local, NOT under $HOME/.local as the old workflow did.
  # These smokes run the harness INSIDE the constructed root, which binds the
  # read-only OS surface (/usr, /bin, /etc, /opt) plus the sandbox workspace —
  # and nothing else. A binary in the runner's home is simply absent in there,
  # and the harness fails with "not found", which reads like a broken smoke
  # rather than a path that was never granted.
  sudo install -d -m 0755 /usr/local/share/claude/versions
  sudo install -m 0755 "$binary" \
    "/usr/local/share/claude/versions/${HARNESS_CLAUDE_VERSION}"
  sudo ln -sfn "/usr/local/share/claude/versions/${HARNESS_CLAUDE_VERSION}" \
    /usr/local/bin/claude
  claude --version
  claude --version | grep -qF "$HARNESS_CLAUDE_VERSION" || {
    smoke::error "claude is not at the pinned version ${HARNESS_CLAUDE_VERSION}"
    return 1
  }
}

harnesses::install_opencode() {
  smoke::log "Installing pinned OpenCode ${HARNESS_OPENCODE_VERSION}"
  npm install --global "opencode-ai@${HARNESS_OPENCODE_VERSION}"
  # Must resolve under the read-only OS surface the constructed root binds, for
  # the same reason Claude and Codex must: a binary in the runner's home is
  # simply absent inside the sandbox, and the launch fails with "not found",
  # which reads like a broken smoke rather than a path that was never granted.
  local resolved
  resolved="$(command -v opencode)"
  case "$resolved" in
    /usr/*|/opt/*) ;;
    *)
      smoke::error "opencode resolved to $resolved, which the sandbox root does not bind"
      return 1
      ;;
  esac
  opencode --version
  opencode --version | grep -qF "$HARNESS_OPENCODE_VERSION" || {
    smoke::error "opencode is not at the pinned version ${HARNESS_OPENCODE_VERSION}"
    return 1
  }
}
