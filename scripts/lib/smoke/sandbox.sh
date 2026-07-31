#!/usr/bin/env bash
# The sandbox floor's own host prerequisite, shared by every shard that builds
# one.
#
# It sits here rather than beside a shard's harness pins because it is about
# bubblewrap and the runner image, not about any harness: a shard that launches
# no harness at all still needs it.

# smoke::unlock_userns makes unprivileged user namespaces available, which
# bubblewrap needs and the hosted image restricts by default. Failure is fatal:
# without it every flow would fail to build a floor, which must not be
# mistakable for a boundary that refused something.
smoke::unlock_userns() {
  smoke::log "Unlocking unprivileged user namespaces"
  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 >/dev/null 2>&1 || true
  if ! bwrap --die-with-parent --ro-bind / / --dev /dev --proc /proc \
       --tmpfs /tmp -- true; then
    smoke::error "bubblewrap cannot create a sandbox on this runner; the image or its AppArmor policy changed"
    return 1
  fi
}
