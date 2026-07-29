# Linux network filtering in `tclaude-layer`

**Audience:** operators and contributors who want to understand how tclaude's
own Linux sandbox controls network egress.

This guide is specifically about
`--sandbox-impl tclaude-layer`. It does not describe Claude Code's sandbox,
Codex's managed sandbox or proxy, or the permission rules either harness
applies to tool calls. In this mode tclaude wraps the complete tool-executing
harness process in its own Linux boundary.

Filtered `list` enforcement is currently enabled for exact `tclaude-layer`
Claude Code, Codex, and OpenCode launches. The `stacked` implementation does
not claim this filtered-network boundary.

For the filesystem side of that boundary, profile composition, and the
differences between sandbox implementations, read
[How sandboxing works](sandboxing.md).

## The short version

Filtered networking uses four Linux building blocks:

1. **bubblewrap** creates private user, network, PID, and mount namespaces.
2. **nftables** installs a default-drop output policy inside the private
   network namespace.
3. **pasta** supplies rootless outbound connectivity without exposing inbound
   ports or the host network namespace.
4. A **tclaude DNS broker** admits only authored names and adds their returned
   addresses to per-rule nftables sets for the DNS TTL.

The resulting path is:

```text
harness process
      │
      │ TCP/UDP packet
      ▼
private network namespace
      │
      ├── nftables output policy ── denied
      │
      └── allowed
             │
             ▼
         rootless pasta
             │
             ▼
         host network
```

`pasta` provides the route, but it does not decide policy. The namespace-local
nftables output chain is the packet floor in front of that route.

## Network postures

The resolved `network.mode` maps to one of three tclaude-layer postures:

| Profile mode | Linux behavior |
|---|---|
| omitted or `open` | Keep the host network namespace. There is no tclaude egress filter. |
| `closed` | Create a private network namespace with sandbox-private loopback and no external route. |
| `list` | Create a private network namespace, install the filtered nftables policy, and attach the supervised DNS and `pasta` gateway. |

The legacy `network_access: internet` and `network_access: none` spellings map
to `open` and `closed`, respectively. A list has no legacy spelling.

The constructed-root postures also isolate PIDs and pathname Unix sockets.
Unix-socket access is a separate filesystem/mount concern; nftables does not
filter `AF_UNIX`. The canonical agentd socket is bind-mounted as a
non-removable control-plane exception.

## Authoring egress targets

A network allow entry selects exactly one destination kind:

```json
{
  "network": {
    "mode": "list",
    "allow": [
      {
        "host": "api.github.com",
        "ports": [443]
      },
      {
        "domain": "example.com",
        "include_subdomains": true,
        "ports": [443]
      },
      {
        "cidr": "10.20.0.0/16",
        "ports": [5432]
      },
      {
        "loopback": true,
        "ports": [3000]
      }
    ]
  }
}
```

Selectors mean:

| Selector | Authority |
|---|---|
| `host` | One exact DNS name. |
| `domain` | The exact domain apex, plus label-bound children when `include_subdomains` is true. |
| `cidr` | Direct IPv4 or IPv6 packet destinations in the prefix. It grants no DNS-name authority. |
| `loopback` | The explicit profile spelling for host loopback through the synthetic `host.tclaude.internal` mapping. It does not expose host loopback as `127.0.0.1` or `::1`. See the synthetic-address reservation gap below. |

`ports` apply to both TCP and UDP. QUIC is therefore covered as UDP. An omitted
or empty port list permits every TCP and UDP destination port for that
selector. Raw sockets, packet sockets, and general ICMP are not authored
connection classes.

Host and domain values are normalized to ASCII DNS names. Suffix matching is
label-bound, so `badexample.com` does not match `example.com`. IP literals must
use `cidr`, and the real IPv4/IPv6 loopback ranges must use the dedicated
`loopback` selector. The two synthetic addresses used by `pasta` are not
currently rejected from CIDR or DNS-derived rules; see
[Host-loopback mapping and current reservation gap](#host-loopback-mapping-and-current-reservation-gap).

### Composition across profiles

Global, group, and explicit network rules compose by intersection and never
widen one another:

- omitted is absorbed;
- `closed` dominates;
- `open` yields to the more restrictive side;
- two lists keep only their overlapping selectors and ports.

Effective deny rows compose independently as a union. Equivalently,
`(B1 - D1) ∩ (B2 - D2) = (B1 ∩ B2) - (D1 ∪ D2)`: allow authority intersects,
deny authority accumulates, deny wins an overlap, and authoring order cannot
change the result.

For example, a global rule allowing `example.com` and all its subdomains,
intersected with an explicit rule allowing only `api.example.com:443`, becomes
the exact host and port rule. Disjoint lists produce an empty list, which
allows no new external flow.

Network denies are active only for Claude Code and Codex launches using the
Linux `tclaude-layer` filtered gateway. Other implementation, harness, and
platform cells omit each deny row individually with a persisted disclosure;
an unsupported port-scoped row is never widened into a whole-destination
block. OpenCode remains outside this deny activation.

CIDR and host-loopback denies are direct packet rules. DNS-name denies are
reported as fully enforced under the default-deny/list posture. Under
default-allow they are partial: the sandbox DNS broker installs negative
address leases before releasing an answer, but another address for the same
service or encrypted DNS that bypasses the broker can remain reachable. A
blocked shared address also affects other names until that lease expires. The
dashboard shows these target-specific outcomes per effective rule in the
policy preview rather than attaching verdicts to authored rows.

## Compiling the packet policy

Each filtered launch compiles the resolved policy into an nftables batch. The
batch:

- flushes the fresh namespace's ruleset;
- creates an `inet tclaude_filter` table;
- creates separate timed IPv4 and IPv6 sets for every host/domain rule;
- installs an output base chain with the resolved default verdict;
- permits sandbox-private loopback;
- permits only the IPv6 neighbor/router discovery needed to reach the
  namespace gateway;
- emits TCP and UDP drop rules for each denied CIDR, host-loopback mapping, or
  negative DNS set;
- permits established, but not generically related, conntrack traffic;
- emits TCP and UDP accept rules for each CIDR, host-loopback mapping, or DNS
  set, narrowed by the authored ports.

Deny rules are before established-flow acceptance and allow rules. A static
CIDR deny therefore defeats every overlapping allow. A newly observed denied
DNS answer installs a negative timed-set lease at that same priority and cuts
matching established TCP/UDP authority as well as new flows.

Conceptually, a host rule becomes:

```text
set dns4_N { type ipv4_addr; flags timeout; }
set dns6_N { type ipv6_addr; flags timeout; }

ip  daddr @dns4_N tcp dport 443 accept
ip  daddr @dns4_N udp dport 443 accept
ip6 daddr @dns6_N tcp dport 443 accept
ip6 daddr @dns6_N udp dport 443 accept
```

The sets start empty. DNS answers populate them later. Separate sets per
authored entry preserve port distinctions when two names resolve to the same
address.

Without a matching deny, `ct state established accept` deliberately lets a
connection admitted while a positive DNS lease was current continue after that
lease expires. A new flow must match current authority. A negative DNS lease is
different: its earlier drop rule cuts the matching established flow for the
negative lease lifetime. The policy does not accept all `related` traffic: a
protocol-created side channel must satisfy its own destination and port rule.

## Gated launch sequence

The route is not made available until the complete default-drop policy is
active:

```text
resolve and validate profile
          │
          ▼
render nft batch and sealed launch artifacts
          │
          ▼
bubblewrap creates private namespaces
          │
          ▼
trusted bootstrap binds private DNS sockets
          │
          ▼
install complete nft policy atomically
          │
          ▼
hand DNS and netfilter descriptors to supervisor
          │
          ▼
start DNS broker and pasta; verify pasta readiness
          │
          ▼
drop every capability and exec the harness
```

### Rootless namespace authority

For filtered mode, rootless bubblewrap maps the invoking host user to UID/GID
0 in a new user namespace. This is not host root. Host files still belong to
the invoking user, and files created by the harness map back to that host
UID/GID.

The namespace bootstrap temporarily receives:

- `CAP_NET_BIND_SERVICE`, to bind the private DNS listener on port 53;
- `CAP_NET_ADMIN`, to install the namespace-local nftables policy.

Before running `nft`, the bootstrap sets no-new-privileges and narrows its
capability sets so only `CAP_NET_ADMIN` crosses that trusted child exec. Before
executing the harness it clears ambient capabilities, drops every capability
set, verifies the drop, and removes the bootstrap and policy paths.

### Pinned executable and sealed launch data

The outer supervisor opens the running tclaude executable through
`/proc/self/exe`, pinning the selected executable through an open descriptor.
Separately, it creates sealed memory files containing:

- the rendered nftables batch;
- the sandbox `/etc/hosts`;
- the sandbox resolver configuration.

Bubblewrap installs the executable descriptor and sealed data descriptors as
read-only files in the constructed root. The memory-file seals prevent the
three data artifacts from being changed between validation and use.

The bootstrap signals readiness over a launch-private Unix packet socket. The
outer supervisor checks the peer credentials and verifies through `/proc` that
the peer belongs to the expected network namespace.

## DNS as a short-lived capability grant

The sandbox's resolver configuration contains only:

```text
nameserver 127.0.0.53
```

The bootstrap binds UDP and TCP listeners at that address inside the private
namespace, then passes those socket descriptors to the outer tclaude
supervisor. Although the DNS broker runs outside the sandbox, it services
sockets that belong to the sandbox's network namespace.

For an A or AAAA query, the broker:

1. Normalizes the requested name.
2. Finds exact host or label-bound domain rules that cover it.
3. Returns `REFUSED` when no rule matches.
4. Queries the host's configured resolvers for an allowed name.
5. Follows a bounded CNAME chain.
6. Rejects invalid, zero-TTL, excessive, or unsafe address answers.
7. Adds each admitted address to every matching rule's IPv4 or IPv6 nftables
   set.
8. Gives the set element the observed DNS TTL, capped at one hour.
9. Returns the admitted answer to the sandbox.

The bootstrap also passes out a netfilter Netlink descriptor created inside
the namespace. The broker uses only that namespace-owned authority to add or
refresh timed set elements. A fresh DNS answer uses create-or-replace, so only
a new observation refreshes a lease. There is no timer-driven refresh or fixed
grace period.

Host mappings from the host's `/etc/hosts` go through the same broker and
receive a short lease. They are not copied wholesale into the sandbox, which
would bypass name checks and lease expiry. Non-address DNS responses are
sanitized so A, AAAA, SVCB, and HTTPS address-bearing data cannot smuggle
unleased destinations through an auxiliary query.

### What lease expiry means

After an address lease expires:

- a new TCP or UDP flow to that address no longer matches the nft set;
- a fresh lookup can install a new lease;
- an already-established conntrack flow may continue.

That last behavior preserves long-running streams and connected UDP
operations. It is also an explicit limitation: hostname rules are
DNS-to-address authority, not continuous application-identity enforcement.

The packet filter does not inspect TLS SNI, HTTP `Host`, certificates,
repositories, tenants, or API operations. An address returned for an allowed
name can be reused directly, including by another application, until its lease
expires. This matters for providers that use shared IP addresses.

CIDR rules are different: they create static packet authority and never
authorize a DNS query merely because its answer lies in the prefix.

## Host-loopback mapping and current reservation gap

`127.0.0.1` and `::1` remain sandbox-private. They cannot name a service
listening on the host.

For explicit host-loopback access, tclaude uses:

```text
169.254.2.2  host.tclaude.internal
fd00::2      host.tclaude.internal
```

`pasta` maps those synthetic destinations to host loopback. In the intended
and explicit form, nftables applies the ports from an authored `loopback`
rule. Hard-coded `127.0.0.1` and `::1` still cannot escape the namespace.

If an allowed DNS name resolves to a real loopback address, the broker refuses
the answer unless loopback authority is also authored. When it is authorized,
the broker rewrites the answer to the corresponding synthetic address, leaving
the loopback nft rule as the final port authority.

The current implementation does **not** reserve `169.254.2.2` and `fd00::2`
against other selector kinds:

- a CIDR rule that contains either synthetic address can reach host loopback on
  that CIDR rule's ports;
- an allowed host or domain whose DNS answer is either synthetic address can
  place it in that rule's timed nft set and reach host loopback on that rule's
  ports.

The dedicated `loopback` selector is therefore not presently the sole
host-loopback authority. Operators should treat any rule capable of admitting
either synthetic address as host-loopback authority. In particular, a
permitted DNS name controlled by an untrusted party can use this as a DNS
rebinding route to host services on the permitted ports.

## The `pasta` gateway

The outer supervisor starts `pasta` in the foreground and points it at the
bubblewrap child PID so it joins the child's network namespace. The launch
configuration:

- asks `pasta` to configure the namespace network;
- disables host-to-namespace TCP and UDP forwarding;
- disables namespace-forwarded TCP and UDP ports;
- disables automatic gateway and guest-address mappings;
- disables splice shortcuts;
- enables only the fixed synthetic host-loopback mappings;
- writes a PID file used as its readiness proof.

The supervisor checks that the PID file names the exact process it started.
Only after this succeeds does it release the bootstrap to execute the harness.

## Prerequisites and trust checks

Every filtered launch probes the actual host requirements:

- bubblewrap can create the required user, mount, network, and PID namespaces;
- the namespace bootstrap receives the two required namespace-local
  capabilities;
- Linux pidfds are available;
- `pasta` and `nft` resolve to absolute executables;
- each executable and every path component above it is root-owned and not
  group- or world-writable;
- the installed `pasta` exposes every option tclaude relies on to disable
  forwarding, mappings, and splice behavior.

There is no fallback to an older or partially capable gateway.

There is one important policy-level distinction:

- **Before filtered enforcement is selected**, an unavailable prerequisite
  widens the network list to host-open and records a persistent warning that
  the list is not enforced.
- **After the live probe selects filtered enforcement**, setup is gated and
  fail-closed. Failure to install the policy or start the gateway prevents the
  harness from running.

Operators should therefore inspect the resolved launch verdict rather than
treating an editor preview as proof of enforcement.

## Supervision and failure behavior

The outer tclaude relay supervises bubblewrap, `pasta`, and the DNS broker:

- bubblewrap starts with `--die-with-parent`;
- `pasta` receives a parent-death signal;
- the sandbox child is pinned with a pidfd, avoiding PID-reuse races;
- if `pasta` exits unexpectedly, the relay kills the sandbox;
- if the DNS broker fails, the relay kills the sandbox;
- when the sandbox exits, the relay terminates `pasta`;
- the harness cannot start before nftables and gateway readiness.

This makes a gateway failure a sandbox failure, rather than silently leaving a
partially configured process running.

## Security properties and limits

The boundary provides:

- a kernel-enforced default-drop packet floor;
- exact IPv4/IPv6 CIDR and TCP/UDP port enforcement;
- DNS-name admission backed by TTL-bound nftables sets;
- no ambient host network namespace;
- no inbound `pasta` forwarding;
- no hidden model-transport bypass: the complete harness process uses the same
  network namespace.

Its deliberate limits are:

- host/domain identity is DNS-to-IP, not application identity;
- shared resolved IPs are reusable for the lifetime of the lease;
- established flows survive lease expiry;
- a port rule covers both TCP and UDP;
- raw/packet sockets and general ICMP are outside the authored contract;
- pathname Unix sockets are governed by the constructed filesystem root and
  bind mounts, not by nftables;
- the synthetic host-loopback addresses are not reserved from CIDR and
  DNS-derived rules, so the dedicated `loopback` selector is not exclusive;
- missing launch prerequisites degrade the authored list to host-open with a
  recorded warning before enforcement is selected.

For the operator-facing summary, model-endpoint gating, and the wider
filesystem/socket boundary, see
[Isolated-with-agentd network posture](sandboxing.md#isolated-with-agentd-network-posture).

## Code map

The main implementation seams are:

| Area | Source |
|---|---|
| Profile schema, validation, and intersection | `pkg/claude/common/sandboxpolicy/access_rules.go` |
| Filtered rule intermediate representation | `pkg/claude/common/sandboxpolicy/filtered_network.go` |
| nftables policy rendering | `pkg/claude/common/sandboxpolicy/filtered_network_nft.go` |
| Network posture and bubblewrap construction | `pkg/claude/session/sandbox_bwrap.go` |
| Host prerequisite and executable trust probes | `pkg/claude/session/filtered_network_probe_linux.go` |
| Bootstrap, descriptor handoff, and `pasta` launch | `pkg/claude/session/filtered_network_gateway_linux.go` |
| DNS matching, resolution, and nft lease updates | `pkg/claude/session/filtered_network_dns_linux.go` |
| Process supervision and fail-closed teardown | `pkg/claude/session/sandbox_bwrap_winch_relay_linux.go` |
