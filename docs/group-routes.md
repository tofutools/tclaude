# Group routes

Group routes are explicit, named TCP capabilities between members of one
agent group. A publisher names a loopback target; a current group member opens
that named route and receives an opaque byte stream. The route is an authority
record, not a general network bridge.

```bash
tclaude routes publish --group <group> --name api --target tcp://127.0.0.1:43130
tclaude routes open --group <group> --route <publisher-agent>/api
tclaude routes ls --group <group>
```

Routes are opt-in per launch and fail closed. The launch must have the current
agent identity, launch generation, group generation, and route-helper
credential. Membership changes invalidate old group generations; stale or
unpublished routes, wrong-group opens, and publisher exit are refused or
withdrawn. Targets are loopback-only. There is no UDP, ambient mesh, peer
discovery, arbitrary host/Internet access, or route-based credential sharing.
Ordinary agent messaging remains a separate control-plane path.

## Platform capability matrix

| Platform | Activation | Contract and disclosure |
|---|---|---|
| Linux | **Full** when the route-capable `tclaude-layer` launch and namespace-local helper are active | The helper reaches agentd through the authenticated Unix relay. Only the named route's opaque TCP stream crosses the boundary; the launch retains its existing provider/host policy floor. |
| macOS | **Partial** | Seatbelt admits a bounded, exact TCP slot pool per route-capable launch. The host-wide localhost model means same-port local reachability remains a documented limitation; the dashboard and launch evidence say `Partial`. |
| Other platforms | **Unavailable** | Route activation returns an explicit unsupported-platform error. It does not silently create a route with a weaker boundary. |

The authoritative activation cells run through the production API, launch, and
adapter paths. The Linux cell runs a publisher and consumer in real Bubblewrap
network namespaces and proves opaque traffic, policy-floor denial, stale and
wrong-group refusal, sustained ordinary messaging, publisher withdrawal, and
publisher-exit withdrawal. The Darwin cell runs the production `session.RunNew`
and Seatbelt path and proves exact-slot authorization, the same negative and
lifecycle cases, opaque traffic, sustained ordinary messaging, and the
documented `Partial` result. Both cells assert the exact checked-out commit.

The evidence workflow is
`.github/workflows/group-route-feasibility.yml`; the older feasibility probes
remain useful as a lower-level boundary check, while the `TCL-952` production
cells are the activation evidence.

## Dashboard and launch disclosure

The Groups **Route map** is a read-only projection of current routes, leases,
health, stale/wrong-group boundaries, and Darwin capacity. It is deliberately
off by default. Enable it in **Config → Experimental features → Groups Route
map**, stored as `features.groups_route_map`; changing it affects the next
dashboard refresh and does not grant route authority.

When the route capability is active, launch/session details disclose the
platform contract and current route state. A Darwin launch must continue to
show `Partial`; a missing or stale launch contract must not be presented as
active. The route map never displays helper credentials or raw authenticated
socket material.

## Troubleshooting

* `route_generation_stale`: restart or relaunch the affected agent so its
  launch generation and group generation are current. Do not reuse an old
  helper credential.
* `route_not_found` or `route_not_member`: use the exact current route
  reference and verify that publisher and consumer are members of the same
  current group roster.
* A lease remains `pending`: confirm the route-capable launch is active and
  that its helper can reach the agentd Unix relay. A pending lease is not a
  permission to connect directly to the publisher target.
* `publisher-lost` or a closed lease: the publisher exited or withdrew the
  route. Publish a new named route from a current launch.
* An unsupported-platform error is expected outside Linux and macOS. Do not
  widen network policy to work around it.

For a security or boundary question, start with [Sandboxing](sandboxing.md),
then inspect the exact evidence artifact for the checked-out head. A green
dashboard Route map only describes current authority; it does not prove that
an old launch, arbitrary target, or different group can connect.
