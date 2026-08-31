#!/usr/bin/env bash
# The PACKET floor's host prerequisites: a trusted `pasta` and `nft`.
#
# It lives in the shared lib rather than in one shard because two different
# shards need the same floor for different reasons, and because ci.yml's
# sandbox-v2 job currently carries a copy of this inline. Nothing here reads a
# workflow variable or an Actions-only path, so the workflow can share the
# apt-source isolation helper without coupling this packet-floor pin to
# Actions-only paths. The pin remains inline there so its trust-walk setup stays
# visible in the job that exercises it.
#
# WHY A SHARD WITHOUT A PROXY NEEDS THIS: a loopback-only list is a FILTERED
# posture that is not discriminating, so it deploys no filtering engine and the
# launch runs the packet gateway's floor. Proving "this policy deploys no proxy"
# end to end therefore means actually building that floor.
#
# # Failure attribution
#
# Every failure here is reported as a PREREQUISITE failure, with the
# SMOKE_PACKET_FLOOR_PREREQ_MARKER text and its own step-summary block. That
# separation is the point: an upstream passt build break, a moved release tag or
# an untrusted runner path must never be readable as "the sandbox stopped
# enforcing the posture". A caller that cannot tell those apart would eventually
# have someone debug a policy regression that never happened.

# The exact upstream release the filtered gateway is exercised against. Ubuntu's
# packaged pasta predates the synthetic host-loopback controls the gateway
# needs, so the smokes build a reproducible prerequisite from source.
SMOKE_PASST_TAG="2026_07_28.f8df3f1"
SMOKE_PASST_COMMIT="f8df3f1b228fe19a74a269334fdfe6cc7d0605ce"
SMOKE_PASST_ARCHIVE_URL="https://passt.top/passt/snapshot/passt-${SMOKE_PASST_TAG}.tar.xz"
SMOKE_PASST_ARCHIVE_SHA256="fcfeb5fbdf775bcc48edc1d5eac8a6d57bc333f8e67b714149376d36061416f0"

# The install prefix is dedicated and root-owned on purpose: hosted runners
# deliberately leave /usr/local/bin writable, which must FAIL tclaude's
# production executable trust walk. A pasta-only prefix also avoids shadowing
# the Actions-managed Go toolchain with /usr/bin/go.
SMOKE_PACKET_FLOOR_PREFIX="/usr/lib/tclaude-filtered"

SMOKE_PACKET_FLOOR_PREREQ_MARKER="packet-floor prerequisite failed"

# smoke::packet_floor_prereq_failed REASON — one attribution point, so a caller
# never has to phrase this itself and every shard says it the same way.
smoke::packet_floor_prereq_failed() {
  smoke::error "$SMOKE_PACKET_FLOOR_PREREQ_MARKER: $*"
  {
    printf '### Packet-floor prerequisite failed\n\n'
    printf 'The host could not be given a trusted `pasta`/`nft`, so the launch that needs the packet floor was NOT run.\n\n'
    printf 'This is a PREREQUISITE failure, not a posture regression: no policy was evaluated and no enforcement claim was falsified.\n\n'
    printf 'Reason: `%s`\n\n' "$*"
    printf 'Pinned passt release: `%s` (`%s`)\n' "$SMOKE_PASST_TAG" "$SMOKE_PASST_COMMIT"
  } | smoke::summary
  return 1
}

# smoke::packet_floor_install provisions the floor's prerequisites and puts the
# trusted pasta first on PATH for this process and everything it launches.
#
# It is idempotent: a second call re-asserts rather than rebuilds.
smoke::packet_floor_install() {
  local tmp="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
  local source_dir="$tmp/passt"
  local bin_dir="$SMOKE_PACKET_FLOOR_PREFIX/bin"

  if [[ ! -x "$bin_dir/pasta" ]]; then
    smoke::log "Installing packet-floor prerequisites (nftables, passt $SMOKE_PASST_TAG)"
    if [[ "${SMOKE_SKIP_APT:-0}" != "1" ]]; then
      smoke::apt_update ||
        { smoke::packet_floor_prereq_failed "apt-get update failed"; return 1; }
      sudo apt-get install --yes --no-install-recommends nftables ||
        { smoke::packet_floor_prereq_failed "installing nftables failed"; return 1; }
    fi

    if [[ ! -d "$source_dir" ]]; then
      smoke::download_extract_tar_xz \
        "$SMOKE_PASST_ARCHIVE_URL" "$SMOKE_PASST_ARCHIVE_SHA256" "$source_dir" ||
        { smoke::packet_floor_prereq_failed "downloading passt $SMOKE_PASST_TAG source failed"; return 1; }
    fi
    make -C "$source_dir" pasta ||
      { smoke::packet_floor_prereq_failed "building pasta from source failed"; return 1; }

    sudo install -d -o root -g root -m 0755 \
      "$SMOKE_PACKET_FLOOR_PREFIX" "$bin_dir" ||
      { smoke::packet_floor_prereq_failed "creating $bin_dir failed"; return 1; }
    sudo install -o root -g root -m 0755 \
      "$source_dir/passt" "$bin_dir/pasta" ||
      { smoke::packet_floor_prereq_failed "installing pasta into $bin_dir failed"; return 1; }
  fi

  # The hosted image also carries an older /usr/local/bin/pasta, so the
  # dedicated path is PREPENDED and the resolution is asserted — the same
  # resolution the production prerequisite performs.
  export PATH="$bin_dir:$PATH"
  if [[ -n "${GITHUB_PATH:-}" ]]; then
    printf '%s\n' "$bin_dir" >> "$GITHUB_PATH"
  fi
  local resolved
  resolved="$(command -v pasta || true)"
  if [[ "$resolved" != "$bin_dir/pasta" ]]; then
    smoke::packet_floor_prereq_failed \
      "pasta resolves to '${resolved:-nothing}', not $bin_dir/pasta"
    return 1
  fi
  pasta --version ||
    { smoke::packet_floor_prereq_failed "the installed pasta does not run"; return 1; }
  command -v nft >/dev/null 2>&1 ||
    { smoke::packet_floor_prereq_failed "nft is not installed"; return 1; }
}
