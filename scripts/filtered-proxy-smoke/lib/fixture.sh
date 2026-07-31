#!/usr/bin/env bash
# Live network fixtures for the filtered-proxy smokes.
#
# Each flow provisions its OWN namespace on its OWN subnet. That independence
# is deliberate: flows must not be able to interfere with each other, and a
# flow must not silently depend on a fixture some earlier flow happened to
# leave behind.
#
# Every fixture is proven with a direct round trip BEFORE any sandbox is built.
# The smokes assert that denied destinations are refused, and a refusal is only
# evidence if the destination was reachable in the first place — otherwise a
# torn-down fixture and a working policy are indistinguishable.

# fixture::netns_up NAMESPACE HOST_LINK PEER_LINK HOST_CIDR ADDR...
fixture::netns_up() {
  local ns="$1" host_link="$2" peer_link="$3" host_cidr="$4"
  shift 4
  sudo ip netns add "$ns"
  sudo ip link add "$host_link" type veth peer name "$peer_link"
  sudo ip link set "$peer_link" netns "$ns"
  # Suppress kernel-generated IPv6 link-local addresses: these fixtures are
  # IPv4 and an unasked-for address is one more thing that can answer.
  sudo ip link set dev "$host_link" addrgenmode none
  sudo ip netns exec "$ns" ip link set dev "$peer_link" addrgenmode none
  sudo ip link set "$host_link" up
  sudo ip address add "$host_cidr" dev "$host_link"
  sudo ip netns exec "$ns" ip link set lo up
  sudo ip netns exec "$ns" ip link set "$peer_link" up
  local addr
  for addr in "$@"; do
    sudo ip netns exec "$ns" ip address add "$addr" dev "$peer_link"
  done
}

# fixture::netns_down NAMESPACE HOST_LINK — safe to call when nothing was made.
fixture::netns_down() {
  sudo ip netns del "$1" 2>/dev/null || true
  sudo ip link del "$2" 2>/dev/null || true
}

# fixture::serve NAMESPACE ADDRESS PORT LOG — an echo listener inside the
# namespace. Echo rather than a protocol server: the smokes assert carriage and
# refusal, not application behavior, and an echo makes the round-trip proof
# below trivially checkable.
fixture::serve() {
  local ns="$1" address="$2" port="$3" log="$4"
  sudo ip netns exec "$ns" \
    socat "TCP4-LISTEN:$port,bind=$address,reuseaddr,fork" EXEC:/bin/cat \
    >"$log" 2>&1 &
  printf '%s' "$!"
}

# fixture::prove_reachable ADDRESS PORT — the anti-vacuous precondition.
fixture::prove_reachable() {
  local address="$1" port="$2" token reply
  token="fixture-$address-$port"
  reply="$(printf '%s' "$token" | timeout 5 socat -T 2 -t 2 - "TCP4:$address:$port" || true)"
  if [[ "$reply" != "$token" ]]; then
    smoke::error "fixture failed a direct round trip to $address:$port; a refusal here would prove nothing"
    return 1
  fi
}

# fixture::hosts_add LINE... — appends to the host resolver. The proxy resolves
# names HOST-side, so fixture names must live in the host's /etc/hosts.
#
# The backup is taken once, and restoring it is the caller's job through an
# EXIT trap. Getting that wrong is not a tidiness bug: flows share hostnames,
# glibc returns the FIRST match, so a stale entry left by one flow silently
# redirects the next flow to the wrong address and fails it for a fabricated
# reason. run.sh also restores on EXIT/INT/TERM so a cancelled job cannot leave
# the runner's resolver rewritten.
fixture::hosts_add() {
  local backup="${SMOKE_HOSTS_BACKUP:?fixture::hosts_add needs SMOKE_HOSTS_BACKUP}"
  [[ -f "$backup" ]] || sudo cp /etc/hosts "$backup"
  printf '%s\n' "$@" | sudo tee -a /etc/hosts >/dev/null
}

fixture::hosts_restore() {
  local backup="${SMOKE_HOSTS_BACKUP:-}"
  [[ -n "$backup" && -f "$backup" ]] || return 0
  sudo tee /etc/hosts <"$backup" >/dev/null || true
}
