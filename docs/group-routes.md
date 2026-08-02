# Group routes

Group routes are explicit, named TCP capabilities between members of one agent
group. A publisher names a loopback target; a current group member opens that
named route and receives an opaque byte stream. The route is an authority
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
| Linux | **Full** when the route-capable launch and namespace-local helper are active | The helper reaches agentd through the authenticated Unix relay. Only the named route's opaque TCP stream crosses the boundary; the launch retains its existing provider/host policy floor. |
| macOS | **Not activated** | Seatbelt admits a bounded, exact TCP slot pool per route-capable launch, and lower-level feasibility evidence exists, but Darwin production activation is not claimed here. Treat macOS as unactivated until its own closeout lands. |
| Other platforms | **Unavailable** | Route activation returns an explicit unsupported-platform error. It does not silently create a route with a weaker boundary. |

## Linux activation evidence

The authoritative Linux cell runs through the production API, the authenticated
Unix relay, and `routeadapter`. `TestLinuxRouteCapabilityIntegratedSmoke` starts
a publisher and a consumer in real unshared Bubblewrap network namespaces and
proves, against the exact checked-out commit:

* the namespace policy floor still denies the host control listener and the
  Internet after the route capability is active;
* opaque TCP flows end to end over the named route;
* an unpublished neighbour, a wrong-group open, a stale group generation, a
  stale launch generation, and a non-loopback target are all refused before any
  data channel is admitted;
* ordinary agent messaging stays accepted and readable while route traffic runs;
* explicit withdrawal closes attached channels;
* a generation-bound publisher exit withdraws the route and closes the lease,
  the broker channel, and the consumer endpoint.

The cell runs in `.github/workflows/group-route-feasibility.yml`, which asserts
the requested checkout before running and uploads the evidence log. The older
probes under `scripts/group-route-feasibility/` remain useful as a lower-level
boundary check.

### Stage evidence

Each namespace child publishes a bounded `stage:` marker as it crosses a
production boundary — control-file hand-off, authenticated endpoint status,
Unix dial, HTTP upgrade, and broker attach — and publishes `stage-failed:` with
the failing boundary before it exits. The host cell fails immediately on a
published stage failure and logs the ordered stage list on success, so a stalled
helper names the boundary it stalled on instead of expiring as an anonymous
timeout. Every child wait is bounded strictly below the host's marker deadline,
and a control path that was never handed to the child fails at once rather than
consuming the wait budget.

## Dashboard and launch disclosure

The Groups **Route map** is a read-only projection of current routes, leases,
health, and stale/wrong-group boundaries. It is deliberately off by default.
Enable it in **Config → Experimental features → Groups Route map**, stored as
`features.groups_route_map`; changing it affects the next dashboard refresh and
does not grant route authority.

When the route capability is active, launch/session details disclose the
platform contract and current route state. A missing or stale launch contract
must not be presented as active. The route map never displays helper credentials
or raw authenticated socket material.

## Troubleshooting

* `route_generation_stale`: restart or relaunch the affected agent so its launch
  generation and group generation are current. Do not reuse an old helper
  credential.
* `route_not_found` or `route_not_member`: use the exact current route reference
  and verify that publisher and consumer are members of the same current group
  roster.
* A lease remains `pending`: confirm the route-capable launch is active and that
  its helper can reach the agentd Unix relay. A pending lease is not a
  permission to connect directly to the publisher target.
* `publisher-lost` or a closed lease: the publisher exited or withdrew the
  route. Publish a new named route from a current launch.
* An unsupported-platform error is expected outside Linux and macOS. Do not
  widen network policy to work around it.

For a security or boundary question, start with [Sandboxing](sandboxing.md),
then inspect the exact evidence artifact for the checked-out head. A green
dashboard Route map only describes current authority; it does not prove that an
old launch, arbitrary target, or different group can connect.
