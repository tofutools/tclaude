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

A route can also be opened from another tclaude instance through
[federation](federation.md#remote-group-routes). The publisher’s operator
grants the consuming peer `routes.consume` on the publishing group; the
consumer’s operator grants its agent `routes.consume` scoped to that peer/group:
`tclaude agent routes open <publisher>/<name>@<peer> -g <group>`.

On Linux, the group must also opt into the route contract through its **group
permissions**: in the dashboard, open the group's **⚙ → group permissions** and
add `routes.publish` (or `routes.consume` for a consuming group). An individual
agent grant authorizes route operations but does not enable the group's launch
contract. Relaunch the publisher after enabling the group policy so its sandbox
gets a namespace-local route helper. Start the service again in that new launch
before publishing its port. Host-only consumption needs no `routes.consume` grant.
A group grant applies to all members. Route-enabled groups also require all
members to be offline before changing group membership.

## Publish only to the host

An agent can publish for operator access without joining or sharing with a group:

```bash
tclaude agent routes publish web --host -t tcp://127.0.0.1:8080
tclaude agent routes ls --host
tclaude agent routes close <route-id>
```

`--host` and `--group` are mutually exclusive; one is required for publication.
Host-only publication requires **`routes.publish-host`**, a separate, unscoped permission that is not
provided by `routes.publish`. Grant it to the agent (or through a profile, default, or group permission)
using the normal permission controls. It does not grant group publication or consumption. No group
membership or group-level route opt-in is required for a directly granted host-only publisher.

Grant the permission before launching, or relaunch after adding it. Host publishing uses the same
pane-authoritative tclaude sandbox launch and publisher helper as group routes. On Linux, the helper
is provisioned from the agent's effective host-publishing permission; on macOS, the launch also
reserves its normal bounded Darwin route slots. Unsupported launch shapes fail closed.

Host-only routes appear in the operator's Route map, including when there are no groups; select one
and click **Open on host** for an HTTP preview or **Open TCP endpoint** for opaque forwarding.
Publishing registers the route; it does not itself allocate a host listener. Agent callers can list
and withdraw their own publications, but cannot read another agent's host-only routes or consume
these routes. They are absent from group route listings and federation route catalogs.

Withdrawing the route, losing the publisher launch, or revoking `routes.publish-host` closes its
host endpoints and active connections. Host listeners remain ephemeral across daemon restarts.

## Open a published route on the host

In the dashboard, enable **Config → Experimental features → Groups Route map**,
select a local route under **Groups → Route map**, and click **Open on host**.
The detail panel shows the host endpoint. For HTTP services, use its preview
link: a unique hostname in the [reserved `.localhost` namespace](https://www.rfc-editor.org/rfc/rfc6761#section-6.3)
keeps dashboard cookies out of the preview request. Raw `127.0.0.1:<port>` endpoints remain
available for curl and other host tools. **Open on host** creates an HTTP preview: agentd rewrites the
HTTP Host header to the published target (for example, `127.0.0.1:8080`), so browser access works
with applications that validate that address. The preview accepts only its displayed hostname,
`localhost`, or `127.0.0.1`, at the preview port; arbitrary Host headers are refused. WebSocket upgrades and streaming responses are supported.
**Open TCP endpoint** creates an opaque TCP lease for other protocols, including HTTPS. Existing leases
keep their original mode; close and reopen a lease to switch modes.

The HTTP preview strips the dashboard session cookie in both directions, while preserving application
cookies. Origin headers, other response headers, redirects, and content remain unchanged. Applications
with separate origin checks or absolute URLs may still need their usual reverse-proxy configuration.
When viewing a remote daemon, the panel shows the daemon-local address and SSH tunnel guidance instead of
linking to the browser machine’s loopback.
**Close host endpoint** releases the listener and its active connections.

This is an operator-originated lease: the authenticated dashboard owns it,
without a broker agent or a `routes.consume` grant. The host-open API is absent
from the agent socket, and an agent cannot create these leases. The publisher
must still explicitly publish the selected port with `routes publish`; host
consumption needs no additional publish flag. Every local host process can
reach the allocated endpoint, so only open services you intend to share locally.
No unlisted publisher ports become reachable through this lease. Each host
lease admits at most eight simultaneous broker streams. Raw TCP endpoints close excess
connections. HTTP previews briefly wait for a stream slot, then return 503 with `Retry-After: 1` if
all slots remain busy (including WebSockets and event streams). HTTP listeners accept at most 32
client connections; idle connections expire after 30 seconds. Host connections still count toward
the route broker’s shared limit.

Host listeners bind IPv4 loopback only and choose an ephemeral port. HTTP previews connect to the
authorized TCP adapter in memory, without exposing an additional listener. The
cookie-authenticated `POST /api/routes/<route-id>/host` API takes `{}` or
`{"port": 43100}` to request a specific loopback port; a busy port is refused. The API defaults to
opaque TCP; pass `{"mode": "http"}` for an HTTP preview, optionally with `port`. Returned leases and
dashboard snapshots include their `mode`.
`DELETE /api/routes/host/<lease-id>` closes it. These endpoints are available
only on the dashboard listener, with its existing session and origin checks.

Host leases appear as **Host (operator)** consumers in the Route map and as
`host` entries in `tclaude agent routes ls` (also `host_leases` in JSON).
The route audit records `host-open` and `host-close`. Leases are ephemeral:
withdrawal, publisher loss, a group generation change, or daemon shutdown
closes them, including idle listeners and existing connections. Reopen after
a daemon restart. Revocation is checked for every broker stream and polled
for idle listeners, normally within 100 ms.

Linux uses the existing publisher namespace helper, preserving closed or
filtered networking. macOS uses the existing publisher adapter and its bounded
launch slots; the host endpoint needs no consumer launch slot. The macOS
host-wide localhost limitation in the platform table still applies. Host binds
exclude ports claimed by sandbox route launches, including idle slots. Federated route mirrors
are not eligible for host consumption.

## What a route is not

Targets are loopback-only. There is no UDP, no ambient mesh networking, no
peer discovery, no arbitrary host or Internet access, and no route-based
credential sharing. Ordinary agent messaging remains a separate
control-plane path and keeps working while route traffic flows. A route
carries exactly one thing: the named target's opaque TCP stream.

## Flow control

Each route connection is flow-controlled: a sender may run only a fixed
window ahead of what its reader has actually taken, so a slow reader slows
the sender down instead of overflowing a buffer and resetting the
connection. Bulk transfers to a reader that pauses work the way they do
over plain TCP. The window is per connection and direction; tune it, or
turn flow control off, in `~/.tclaude/data/config.json`:

```json
{ "routes": { "flow_control": true, "window_kib": 256 } }
```

`window_kib` (default 256, range 256–16384) trades memory per connection
for throughput over slow or distant links such as a
[federation](federation.md) hub. Changes apply to route channels attached
afterwards. It works the same on Linux and macOS. Flow control is
negotiated, so Linux helpers from an older release keep working without
it.

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

The [dashboard](dashboard.md) offers an opt-in **Route map** — a
projection of current routes, leases, health, and stale/wrong-group
boundaries. Enable it under Config → Experimental features → Groups Route
map (`features.groups_route_map`); enabling the map grants no route
authority. Host-open controls require a separate authenticated operator action;
the map never displays helper credentials or raw socket material.

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
- Host open reports "publisher data channel is not attached": a ready route
  record exists, but no publisher helper is carrying its traffic. On Linux,
  verify the group-level route opt-in above, relaunch the publisher, start its
  service, and republish. If its launch already has a current helper, wait for
  that helper to attach and retry opening the host endpoint.
- A lease stays `pending`: confirm the route-capable launch is active and
  its helper can reach the `agentd` Unix relay. A pending lease is not
  permission to connect directly to the publisher's target.
- `publisher-lost` or a closed lease: the publisher exited or withdrew the
  route. Publish a new named route from a current launch.
- An unsupported-platform error is expected outside Linux and macOS. Do not
  widen network policy to work around it.
