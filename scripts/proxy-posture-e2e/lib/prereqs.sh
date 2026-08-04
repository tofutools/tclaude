#!/usr/bin/env bash
# Host packages the posture-e2e flows need.
#
# Deliberately the same short list the proxy smokes install, and for the same
# reason: three of the four scenarios build the proxy engine's floor, which
# reaches its namespace through bubblewrap's plain unshare and calls neither
# pasta nor nft.
#
# The fourth scenario does need them — a loopback-only list deploys no engine
# and therefore runs the packet floor — and it installs them ITSELF, through
# scripts/lib/smoke/packet-floor.sh, so that an upstream passt build break fails
# one flow with a prerequisite verdict instead of failing this whole shard.
#
#   bubblewrap  builds the sandbox floor
#   socat       the fixture listeners and their round-trip proofs
#   iproute2    netns and veth setup
#
# go and git come from the runner image and the workflow's toolchain setup;
# run.sh asserts them by name either way.
PREREQ_PACKAGES=(bubblewrap socat iproute2)

prereqs::install() {
  if [[ "${SMOKE_SKIP_APT:-0}" == "1" ]]; then
    smoke::log "Skipping package installation (SMOKE_SKIP_APT=1)"
    return 0
  fi
  smoke::log "Installing host prerequisites: ${PREREQ_PACKAGES[*]}"
  smoke::apt_update
  sudo apt-get install --yes --no-install-recommends "${PREREQ_PACKAGES[@]}"
}
