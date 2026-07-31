#!/usr/bin/env bash
# Host packages the flows need.
#
# These live HERE rather than in the CI job for the same reason the harness
# pins do: needing a new tool must cost a repo edit and a review, not a merge
# from someone holding workflow scope. The job stays checkout / toolchain /
# invoke.
#
# The list is deliberately SHORTER than the packet gateway's sibling smoke,
# which also installs nftables and builds pasta from source. The proxy floor
# reaches its namespace through bubblewrap's plain unshare and calls neither —
# that is the posture's operational headline (§2.5), and installing them here
# would quietly undermine the claim that this floor does not need them.
#
#   bubblewrap  builds the sandbox floor
#   socat       the fixture listeners and their round-trip proofs
#   iproute2    netns and veth setup
#
# go, node, npm, curl and git come from the runner image and the workflow's
# toolchain setup; run.sh asserts all of them by name either way.
PREREQ_PACKAGES=(bubblewrap socat iproute2)

prereqs::install() {
  if [[ "${SMOKE_SKIP_APT:-0}" == "1" ]]; then
    smoke::log "Skipping package installation (SMOKE_SKIP_APT=1)"
    return 0
  fi
  smoke::log "Installing host prerequisites: ${PREREQ_PACKAGES[*]}"
  sudo apt-get update --quiet
  sudo apt-get install --yes --no-install-recommends "${PREREQ_PACKAGES[@]}"
}
