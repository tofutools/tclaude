# Group routes

Sandboxed agents normally cannot open network paths to each other — that is
the point of the sandbox. But some collaborations need a real byte stream: a
backend agent serving an API that a frontend agent's tests hit, a database
one agent hosts and another queries. **Group routes** are the sanctioned
path: explicit, named, opaque TCP capabilities between members of one
[group](agents-and-groups.md). A publisher names a loopback target; a
current member of the same group opens the named route and receives a
consumer-local endpoint. The route is an authority record `agentd` enforces,
not a general network bridge.

```bash
# Publisher: expose the service listening on local port 43130
tclaude agent routes publish api -g dev-team -t tcp://127.0.0.1:43130

# Consumer (same group): open it and get a local endpoint to connect to
tclaude agent routes open backend/api -g dev-team

tclaude agent routes ls
tclaude agent routes close <lease-or-route>
```

`publish` names the route (no `/` allowed in the name) and points it at a
publisher-local TCP endpoint with `-t`; `--transport tcp` is the only
transport. `open` takes a route ID or the message-friendly reference
`<publisher>/<name>` (or `<group-id>/<publisher>/<name>` when the publisher
name alone is ambiguous), waits for the platform adapter, and prints the
consumer-local endpoint plus a lease ID. `close` takes the lease ID printed
by `open`, or a route reference together with `-g`; the daemon works out
whether you own the published route or a consumer lease. All four accept
`--json` where output matters for scripting.

Publishing needs `routes.publish` and opening needs `routes.consume` —
neither is default-granted, and both additionally require *current*
membership in the named group. See
[Permissions and audit](permissions-and-audit.md).

## What a route is not

Targets are loopback-only. There is no UDP, no ambient mesh networking, no
peer discovery, no arbitrary host or Internet access, and no route-based
credential sharing. Ordinary agent messaging remains a separate
control-plane path and keeps working while route traffic flows. A route
carries exactly one thing: the named target's opaque TCP stream.

## Fail-closed by generation

Routes fail closed. Every membership or permission change in a group
advances the group's **route generation**, and a helper presenting a
generation captured before the change is refused as stale — removing an
agent from a group instantly invalidates its route access, on both ends.
Stale or unpublished routes, wrong-group opens, and non-loopback targets are
refused before any data channel is admitted. When a publisher exits, the
route is withdrawn: its leases close, and consumer endpoints go with it.

## Platform support

| Platform | Support | Contract |
|---|---|---|
| Linux | **Full** | A namespace-local helper reaches `agentd` over the authenticated Unix relay. Only the named route's opaque TCP stream crosses the sandbox boundary; the launch's existing network policy floor stays intact. |
| macOS | **Partial** | Seatbelt admits a bounded, exact TCP slot pool per route-capable launch. Because macOS localhost is host-wide, same-port local reachability remains a documented limitation. |
| Other | Unavailable | Route activation returns an explicit unsupported-platform error rather than silently creating a route with a weaker boundary. |

Each platform's claim is backed by its own CI evidence run at the exact
checked-out commit: real sandboxed publishers and consumers proving, per
platform, that traffic flows over the named route, that stale-generation,
unpublished-route, wrong-group, and non-loopback opens are refused, that
publisher exit withdraws everything, and that the sandbox still denies the
host and the Internet while the route is active. Neither platform's evidence
establishes the other's boundary — a Linux pass says nothing about macOS,
and vice versa.

## Disclosure

When the route capability is active, launch and session details disclose the
platform contract and current route state; a macOS launch shows `Partial`,
and a missing or stale contract is never presented as active.

The [dashboard](dashboard.md) offers an opt-in **Route map** — a read-only
projection of current routes, leases, health, and stale/wrong-group
boundaries. Enable it under Config → Experimental features → Groups Route
map (`features.groups_route_map`); enabling the map grants no route
authority, and it never displays helper credentials or raw socket material.

## Relationship to the sandbox network boundary

Routes ride *inside* the sandbox model described in
[Sandboxing](sandboxing.md) and [Network filtering](network-filtering.md),
they do not punch through it. A route-capable launch keeps its network
policy floor: the allowed-host and provider rules still apply, the host
control listener and the Internet stay denied, and the only new reachability
is the named route's stream, admitted through an authenticated,
identity-checked, generation-checked attach. If a route refusal tempts you
to widen an agent's network profile instead — don't; that trades a named,
auditable capability for an ambient one.

## Troubleshooting

- `route_generation_stale`: restart or relaunch the affected agent so its
  launch and group generations are current. Old helper credentials are not
  reusable.
- `route_not_found` / `route_not_member`: use the exact current route
  reference and verify both publisher and consumer are members of the same
  current group roster.
- A lease stays `pending`: confirm the route-capable launch is active and
  its helper can reach the `agentd` Unix relay. A pending lease is not
  permission to connect directly to the publisher's target.
- `publisher-lost` or a closed lease: the publisher exited or withdrew the
  route. Publish a new named route from a current launch.
- An unsupported-platform error is expected outside Linux and macOS. Do not
  widen network policy to work around it.
