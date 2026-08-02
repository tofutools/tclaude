# TCL-947 group-route feasibility evidence

## Background / Purpose

This report records the bounded platform experiment required before the
dynamic group-route design can be treated as an implementation contract. It
tests the two load-bearing assumptions without activating a production route
capability or weakening the existing sandbox floor.

The exact-head CI workflow is
`.github/workflows/group-route-feasibility.yml`. Its artifacts are the
authoritative evidence for Darwin; a local run is useful for iteration only.

## Gate state

The exact-head CI workflow is the feasibility gate. It must pass both platform
arms at the frozen commit, with the checkout assertion proving that the runner
tested the requested commit rather than a synthetic pull-request merge. The
final run URL and SHA are recorded in the PR and Linear evidence comment after
the frozen rerun; this committed report intentionally describes the stable
evidence shape without embedding a self-referential run link. This is evidence
for the bounded contract below, not a production capability activation.

## Linux arm

The probe launches three independent `bwrap --unshare-all` processes. They do
not join one another's namespaces and receive no bridge, nftables rule, or
ambient peer network. A host-side Unix-socket broker is the stand-in for an
agentd-owned broker:

| Observation | Required result | Meaning |
| --- | --- | --- |
| publisher and consumer post-launch loopback listeners | positive | arbitrary target ports are possible inside each namespace |
| opaque bytes through the broker | positive | namespace-local helpers can carry a route data plane |
| third group `OPEN` | negative (`DENY unauthorized-group`) | group authorization remains explicit |
| host control listener and `1.1.1.1:443` from publisher | negative | ordinary host/Internet policy did not widen |
| publisher endpoint from the host | negative | the host cannot directly consume a namespace-local listener |
| network namespace IDs | three distinct IDs, all distinct from host | no ambient peer namespace was reused |

The probe fails closed when any required observation is missing.

Required Linux success markers:

```text
TCL-947 Linux evidence: POSITIVE
linux route: opaque TCP stream carried through host Unix broker; consumer endpoint was created after launch
linux authorization: third group denied; direct publisher endpoint and host control endpoint unavailable from namespaces
linux policy floor: --unshare-all; no namespace join, bridge, nftables rule, or ambient peer network
```

## Darwin arm

The probe renders eight exact TCP slots, which is inside the design's proposed
8–16 range. One slot is used by a post-launch publisher; another is held by an
unsandboxed stand-in broker as the consumer-facing endpoint. A sandboxed
consumer connects to the broker, which forwards the opaque bytes to the
publisher.

| Observation | Required result | Meaning |
| --- | --- | --- |
| rendered exact slots | positive, exactly 8 | a bounded pool is practical in the profile |
| publisher post-launch bind | positive | reserved publisher slots are usable |
| broker-held consumer endpoint | positive | consumer-side forwarding path works |
| second bind of held consumer slot | negative (`EADDRINUSE`) | broker reservation is observable |
| publisher slot collision | negative (`EADDRINUSE`) | application ports cannot be safely pre-held; no workaround is invented |
| neighboring non-reserved port, external TCP, and consumer bind | negative (`EPERM`) | profile remains narrow |
| same-port non-loopback service | explicit `LIMITATION` marker | Seatbelt's `localhost:<port>` selector is host-wide; rating remains Partial |

The host-wide localhost limitation is disclosed rather than hidden behind a
launch-time collision check. Publisher applications must accept a tclaude-
selected slot; broker-held consumer slots are the safer bounded-pool side.

Required Darwin success markers:

```text
TCL-947 Darwin profile: POSITIVE exact-port pool=8 rendered-slots=9
TCL-947 Darwin collision: POSITIVE publisher slot collision=EADDRINUSE; no workaround invented
TCL-947 Darwin localhost: LIMITATION Seatbelt localhost:<port> reached non-loopback <address>:<port>
TCL-947 Darwin evidence: POSITIVE broker-held consumer endpoint=<port> reached publisher slot=<port> with opaque TCP bytes
TCL-947 Darwin negative: non-reserved neighbor=<port> and external TCP were refused by Seatbelt
```

The profile marker's `rendered-slots=9` count includes the eight exact
outbound exceptions plus the one exact publisher `network-bind` exception; the
pool itself is eight slots.

## Recommended contract after a positive gate

Proceed only with an explicit routed model: retain each agent's independent
sandbox, grant one narrow launch-time broker gateway, and authenticate named
routes dynamically by stable group/member/launch identity. On Linux, use
namespace-local helpers and a broker relay for arbitrary loopback targets. On
Darwin, use a pre-authorized bounded exact-port pool, reserve consumer-facing
slots in the broker, disclose the host-wide localhost limitation as Partial,
and do not promise collision-free publisher allocation. Keep UDP and ambient
mesh networking out of the first contract.

If either arm fails, revise the design record and downstream tickets before
implementation.

## M6 activation follow-on

The feasibility probes above remain the lower-level boundary evidence. The
cross-platform production activation cells and their disclosure contract are
documented in [Group routes](group-routes.md) and run in the same workflow:
`TestLinuxRouteCapabilityIntegratedSmoke` exercises Bubblewrap, the production
agentd API, authenticated Unix relay, and `routeadapter`; the Darwin cell
exercises `session.RunNew`, Seatbelt, exact slots, and the production adapter.
Those cells assert the exact checked-out head and cover current-generation
authority, negative route cases, sustained ordinary messaging, and lifecycle
withdrawal. The dashboard Route map remains opt-in via
`features.groups_route_map`.
