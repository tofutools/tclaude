#!/usr/bin/env bash
# The posture-e2e flows' shared fixture shape.
#
# Every scenario gets the SAME live fixture — an allowed address, an adjacent
# one outside the authored prefix, and an RFC1918 one — on its OWN namespace and
# its OWN subnets. Two properties are being bought:
#
#   Independence. Flows must not be able to interfere with each other, and a
#   flow must never silently depend on a fixture an earlier one left behind.
#   Each flow passes its own index, and every address, link name and port is
#   derived from it.
#
#   Comparability. The four scenarios differ ONLY in the authored policy, which
#   is the variable the conditional-deployment ruling turns on. A scenario with
#   a differently-shaped fixture would confound "this policy deploys no proxy"
#   with "this fixture was different".
#
# Reachability of every exported address is proven by a direct round trip BEFORE
# any sandbox is built: the smokes assert that denied destinations are refused,
# and a refusal is only evidence if the destination was reachable to begin with.

# posture::fixture_up INDEX
#
# INDEX must be unique per flow. It selects 198.18.<3+INDEX>.0/24 for the
# reserved-space half, 192.168.<200+INDEX>.0/24 for the private half, and a
# disjoint pair of veth names and ports.
#
# Nothing here is `local`: the caller's EXIT trap reads these names AFTER
# flow::run has returned, and a local would be out of scope by then — `set -u`
# would abort the cleanup and leave the resolver rewritten and the namespace
# behind. flow::run owns its subshell, so plain assignment leaks nothing.
posture::fixture_up() {
  index="$1"
  ns="tclaude-posture-$index"
  host_link="tclpe$((index * 2))"
  peer_link="tclpe$((index * 2 + 1))"
  allowed="198.18.$((3 + index)).10"
  adjacent="198.18.$((3 + index)).200"
  private_addr="192.168.$((200 + index)).10"
  allowed_prefix="198.18.$((3 + index)).0/25"
  allowed_port="$((41030 + index * 10))"
  denied_port="$((41031 + index * 10))"
  pids=()

  fixture::netns_up "$ns" "$host_link" "$peer_link" \
    "198.18.$((3 + index)).1/24" "$allowed/24" "$adjacent/24"
  # The private half rides the same veth: the proxy dials from the HOST, so this
  # address needs a host route, and this is it.
  fixture::host_address_add "$host_link" "192.168.$((200 + index)).1/24"
  sudo ip netns exec "$ns" ip address add "$private_addr/24" dev "$peer_link"

  # The proxy resolves names host-side, so the fixture names live in the HOST's
  # resolver. The allowed and denied names deliberately share ONE address: what
  # separates them is authored name identity, not reachability, which is what
  # makes refusing the denied one meaningful.
  fixture::hosts_add \
    "$allowed allowed.posture.tclaude.test denied.posture.tclaude.test" \
    "$private_addr private.posture.tclaude.test"

  local address port
  for address in "$allowed" "$adjacent" "$private_addr"; do
    for port in "$allowed_port" "$denied_port"; do
      pids+=("$(fixture::serve "$ns" "$address" "$port" \
        "$SMOKE_ARTIFACTS/posture-$index-tcp-$address-$port.log")")
    done
  done
  sleep 1
  for address in "$allowed" "$adjacent" "$private_addr"; do
    for port in "$allowed_port" "$denied_port"; do
      fixture::prove_reachable "$address" "$port"
    done
  done
}

# posture::fixture_down undoes everything posture::fixture_up did. A flow that
# died without cleaning up would leave its /etc/hosts mapping behind, and glibc
# returns the FIRST match — so the next flow would resolve these names to the
# previous flow's address, outside its own prefix, and fail for a fabricated
# reason.
posture::fixture_down() {
  smoke::kill_listener "${pids[@]:-}"
  fixture::netns_down "$ns" "$host_link"
  fixture::hosts_restore
}

# posture::go_test RUN_PATTERN — runs the named end-to-end smokes against the
# fixture this flow just proved.
posture::go_test() {
  TCLAUDE_PROXY_POSTURE_E2E=1 \
  TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY="$SMOKE_TCLAUDE_BINARY" \
  TCLAUDE_FILTERED_ALLOWED_ADDR="$allowed" \
  TCLAUDE_FILTERED_ADJACENT_ADDR="$adjacent" \
  TCLAUDE_FILTERED_ALLOWED_PREFIX="$allowed_prefix" \
  TCLAUDE_FILTERED_ALLOWED_PORT="$allowed_port" \
  TCLAUDE_FILTERED_DENIED_PORT="$denied_port" \
  TCLAUDE_POSTURE_E2E_PRIVATE_ADDR="$private_addr" \
    go test ./pkg/claude/session \
      -run "$1" \
      -count=1 -v -timeout=600s
}
