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

**Pending exact-head CI evidence.** The probe emits explicit positive,
negative, limitation, and failure markers. A green workflow is not sufficient
if either platform arm was skipped or its named evidence was absent.

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
