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
withdrawn. Targets are loopback-only. There is no UDP, ambient mesh
networking, peer discovery, arbitrary host/Internet access, or route-based
credential sharing. Ordinary agent messaging remains a separate control-plane
path.

## Darwin activation boundary

macOS activation is **Partial**. Seatbelt admits a bounded, exact TCP slot pool
per route-capable launch. The host-wide localhost model means same-port local
reachability remains a documented limitation; the dashboard and launch
evidence must continue to disclose `Partial`.

The Darwin M6 activation cell runs the production `session.RunNew`, Seatbelt,
exact-slot allocator, production adapter, and agentd API paths. It proves:

- exact-slot publisher and consumer authorization, including release and
  reuse of slot `45203`;
- stale-generation, unpublished-route, and wrong-group refusal;
- 96/96 ordinary messages while sustained opaque route traffic continues;
- route withdrawal and idle-consumer cleanup;
- publisher-death withdrawal, including closure of the live consumer endpoint;
- provider and host policy-floor preservation, with the documented same-host
  localhost limitation and Internet denial.

The exact-head evidence workflow is
`.github/workflows/group-route-feasibility.yml`. The Darwin production cell
asserts the checked-out commit and its macOS CI artifact is authoritative;
local Linux runs cannot establish this boundary. This closeout makes no Linux
activation claim.

## Dashboard and launch disclosure

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
* An unsupported-platform error is expected outside supported route hosts. Do
  not widen network policy to work around it.

For a security or boundary question, start with [Sandboxing](sandboxing.md),
then inspect the exact evidence artifact for the checked-out head. A green
dashboard Route map only describes current authority; it does not prove that
an old launch, arbitrary target, or different group can connect.
