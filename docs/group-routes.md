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

## Platform capability matrix

| Platform | Activation | Contract and disclosure |
|---|---|---|
| Linux | **Full** when the route-capable launch and namespace-local helper are active | The helper reaches agentd through the authenticated Unix relay. Only the named route's opaque TCP stream crosses the boundary; the launch retains its existing provider/host policy floor. |
| macOS | **Partial** | A bounded, exact Seatbelt TCP slot pool per route-capable launch. Same-port host-local reachability remains a documented limitation. |
| Other platforms | **Unavailable** | Route activation returns an explicit unsupported-platform error. It does not silently create a route with a weaker boundary. |

Each platform's exact-head CI artifact is the authority for that platform's
claim; neither cell establishes the other's boundary.

## Linux activation boundary

Linux activation is **Full**. The namespace-local helper reaches agentd over the
authenticated Unix relay and attaches through an HTTP upgrade that is
authenticated, identity-checked, and generation-checked before the connection is
hijacked. Only the named route's opaque stream crosses the boundary.

The Linux M6 activation cell runs a publisher and a consumer in real unshared
Bubblewrap network namespaces against the production API, relay, and
`routeadapter`. It proves:

- the namespace policy floor still denies the host control listener and the
  Internet after the route capability is active;
- opaque TCP flowing end to end over the named route;
- refusal of an unpublished neighbour, a wrong-group open, a stale group
  generation, a stale launch generation, and a non-loopback target, each before
  any data channel is admitted;
- ordinary agent messaging staying accepted and readable while route traffic
  runs;
- explicit withdrawal closing attached channels;
- a generation-bound publisher exit withdrawing the route and closing the lease,
  the broker channel, and the consumer endpoint.

The Linux production cell asserts the checked-out commit and its Linux CI
artifact is authoritative. This activation makes no Darwin claim.

### Stage evidence

Each namespace child publishes a bounded `stage:` marker as it crosses a
production boundary — control-file hand-off, authenticated endpoint status, Unix
dial, HTTP upgrade, and broker attach — and publishes `stage-failed:` naming the
failing boundary before it exits. The host cell fails immediately on a published
stage failure and logs the ordered stage list on success, so a stalled helper
names the boundary it stalled on instead of expiring as an anonymous timeout.
Every child wait is bounded strictly below the host's marker deadline, and a
control path that was never handed to the child fails at once rather than
consuming the wait budget.

A helper must present the group generation carried by its own route or lease
row. Every membership and permission change advances a group's route generation,
so a generation captured before such a change is refused by the channel endpoint
as a stale publisher or consumer identity.

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

The Groups **Route map** is a read-only projection of current routes, leases,
health, and stale/wrong-group boundaries. It is deliberately off by default.
Enable it in **Config → Experimental features → Groups Route map**, stored as
`features.groups_route_map`; changing it affects the next dashboard refresh and
does not grant route authority.

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
