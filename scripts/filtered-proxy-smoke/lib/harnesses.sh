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

# Where a CI cache may keep pinned harness artifacts between runs. The workflow
# keys its cache on a hash of THIS FILE, which is where the pins live — a
# conservative superset of the pin strings, so an unrelated edit here costs a
# re-download and can never serve an artifact belonging to a different pin.
#
# That is still only a claim about a cache key, not a proof about a file, so
# every install below asserts the version (and, for Claude, the checksum) against
# the artifact it ACTUALLY uses. See install_claude.
HARNESS_CACHE_DIR="${SMOKE_HARNESS_CACHE_DIR:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/smoke-harness-cache}"

# harnesses::known prints the harness names this file can install, derived from
# the functions themselves rather than a hand-written list — so adding an install
# function immediately makes the shard map's harness-coverage guard demand that
# some shard claim it, instead of it being installed by nobody.
#
# The charset matches what a bash function name can hold after the prefix, NOT
# just lowercase letters: a narrower pattern would skip `install_gemini3`
# entirely, which is the one failure this function must never have — an
# undiscovered harness is installed by nobody AND makes the shard line that
# correctly claims it fail as "cannot install".
harnesses::known() {
  local fn
  while read -r _ _ fn; do
    printf '%s\n' "${fn#harnesses::install_}"
  done < <(declare -F | grep -E ' harnesses::install_[A-Za-z0-9_]+$') | sort
}

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
  local binary="$HARNESS_CACHE_DIR/claude-${HARNESS_CLAUDE_VERSION}-${platform}"
  local curl_args=(
    --fail --location --silent --show-error
    --max-time 60 --retry 2 --retry-all-errors
  )
  mkdir -p "$HARNESS_CACHE_DIR"

  # The published checksum is fetched EVERY run, cache hit or not. It is the
  # authority the artifact is judged against, so reading it out of the same
  # cache as the artifact would make the check circular: a bad pair would verify
  # against itself. It is one small JSON document; nothing is saved by caching
  # it.
  curl "${curl_args[@]}" --output "$manifest" \
    "$base/${HARNESS_CLAUDE_VERSION}/manifest.json"
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

  # MATERIALIZE, THEN VERIFY — in that order, and with the verification outside
  # the branch. A CI cache may hand this step a binary nobody in this run
  # downloaded, so a checksum check that only guarded the download path would
  # leave the cache-hit path unverified: exactly the artifact most worth
  # verifying, since it came from somewhere other than the vendor. Whatever file
  # ends up at "$binary" is what gets checked and what gets installed.
  if [[ -s "$binary" ]]; then
    smoke::log "Using cached Claude Code artifact $binary"
  else
    # Renamed only once complete, so an interrupted download cannot be left
    # behind as a plausible-looking short file — and the partial is cleaned up
    # on failure rather than riding into the cache as dead weight under a key
    # that never expires.
    if ! curl "${curl_args[@]}" --output "$binary.part" \
        "$base/${HARNESS_CLAUDE_VERSION}/$platform/claude"; then
      rm -f "$binary.part"
      smoke::error "could not download Claude Code ${HARNESS_CLAUDE_VERSION}"
      return 1
    fi
    mv "$binary.part" "$binary"
  fi
  # Compared as a string rather than piped to `sha256sum --check`: the --check
  # line format splits on a two-space delimiter and has its own backslash
  # escaping, so a cache directory containing a space would make a CORRECT
  # artifact fail as "no properly formatted checksum lines found" — a real
  # failure with an actively misleading diagnosis.
  local got
  got="$(sha256sum "$binary" | cut -d' ' -f1)"
  if [[ "$got" != "$checksum" ]]; then
    # Removed so this runner re-downloads on the next attempt instead of failing
    # on the same local file. NOTE, because it is easy to assume otherwise: this
    # does NOT evict a poisoned GitHub Actions cache entry, which is immutable
    # under its key and will be restored again. Recovering from that means
    # deleting the cache entry (or changing the key by editing this file).
    rm -f "$binary"
    smoke::error "Claude Code ${HARNESS_CLAUDE_VERSION} checksum mismatch: got $got, expected $checksum; the local artifact has been discarded"
    return 1
  fi

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
