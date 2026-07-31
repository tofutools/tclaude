# Proxy network filtering in `tclaude-layer`

**Audience:** operators and contributors who want to understand tclaude's
second Linux egress-filtering engine — the one that filters by the name a
client asks for, rather than by the packets it sends.

This guide is specifically about `--sandbox-impl tclaude-layer` with
`network.engine: "proxy"`. It does not describe Claude Code's sandbox, Codex's
managed sandbox or its own built-in proxy, or the permission rules either
harness applies to tool calls.

The other engine — the default on Linux — is documented in
[Linux network filtering](linux-network-filtering.md). Read that page for the
packet gateway; read this one for the proxy. The two are alternatives, not
layers: one launch runs one engine, and combining them is refused
(`sandbox_bwrap_winch_relay_linux.go`).

For the filesystem side of the boundary, profile composition, and the
differences between sandbox implementations, read
[How sandboxing works](sandboxing.md).

## The short version

The proxy engine uses two things:

1. **An empty network namespace** — bubblewrap's `--unshare-net --unshare-pid`,
   the same floor the isolated posture already builds. Nothing inside it has a
   route anywhere.
2. **A filtering proxy on the host**, reached through a single loopback socket
   the sandbox binds for itself and hands out.

The resulting path is:

```text
harness process
      │
      │ HTTP CONNECT / absolute-form HTTP / SOCKS5
      ▼
127.0.0.1:<ephemeral>  (bound inside the namespace)
      │
      │ the LISTENING DESCRIPTOR crosses out; no route crosses in
      ▼
tclaude filtering proxy (host side)
      │
      ├── requested target not authorized ── 403 / SOCKS reply 0x02
      │
      └── authorized
             │
             ▼
         host network
```

Two properties follow directly from that shape, and they are the reasons to
choose this engine:

- **Names are the authority.** The proxy decides on the host name the client
  asks for, *before* any resolution. There is no address lease, and a shared IP
  address grants no authority.
- **Everything else fails closed.** A program that ignores the proxy
  environment has no route at all. It does not leak; it cannot connect.

The cost is stated just as plainly: CIDR rules only match a target the client
states *as a literal*, UDP and QUIC have no route out at all, and a deny on a
name does not cover a client that asks for that host's address instead. See
[Security properties and limits](#security-properties-and-limits).

## Selecting the engine

`network.engine` is one authored field on `NetworkRules`, orthogonal to
`baseline` / `mode` / `allow` / `deny` / packs:

```json
{
  "network": {
    "engine": "proxy",
    "mode": "list",
    "allow": [
      { "host": "api.github.com", "ports": [443] },
      { "domain": "example.com", "include_subdomains": true, "ports": [443] },
      { "cidr": "10.20.0.0/16", "ports": [5432] },
      { "loopback": true, "ports": [3000] }
    ]
  }
}
```

`engine` names **how** a discriminating rule set is enforced. It never changes
**what** is authored: it cannot widen or narrow the destinations the policy
declares, so two profiles that agree on rules and disagree on engine authorize
the same destinations. What they differ in is mechanism, protocol carriage, and
honest capability rating.

That is a statement about the authored policy, **not** a promise that switching
engines leaves enforcement unchanged. Two ways a switch changes what a target
actually enforces, both documented below and both worth knowing before you edit
the field:

- **Activation.** Proxy cells are activated per harness, platform and sandbox
  implementation. A profile that changes nothing but `engine: "proxy"` on a
  harness that is not activated yet is **widened to open** with a notice — the
  authored rules stop being enforced. See
  [Harness cooperation and activation](#harness-cooperation-and-activation).
- **The private-destination blocker** narrows an authored name whose answer
  lands in reserved space. See
  [Host-side resolution](#host-side-resolution-and-the-private-destination-blocker).

Unset never changes behavior. Unset resolves to today's behavior per platform:
Linux to the packet gateway, macOS to today's macOS behavior. There is no
default-on flip on either platform.

### Composition — most explicit wins

`engine` does **not** intersect the way the access lattice does. It resolves by
precedence:

```text
session-explicit profile  >  group profile  >  global profile
```

Every layer that omits `engine` is absorbed and yields to the next; the most
explicit layer that names one wins outright. There is no refusal, no
intersection, and no strictness comparison — engines are not ordered by
strictness, because packet is stronger on protocol coverage while proxy is
stronger on name identity.

Because a silent mechanism swap would otherwise be possible by design, the
override is disclosed rather than left implicit. The rendered network axis names
the winning engine and the layer it came from, and when a lower-precedence layer
asked for a different engine and lost, the disclosure says so
(`networkEngineCompositionNotice`). The capability cells, per-entry outcomes,
and mechanism string all re-render for the winner, never for the
authored-but-overridden loser.

## When a proxy is actually deployed

The engine is consulted only when the resolved, pack-expanded policy is
**discriminating** — that is, when the policy asks for a distinction between
destinations that the floor cannot express on its own
(`NetworkRulesAreDiscriminating`):

| Resolved policy | Floor | Proxy process |
|---|---|---|
| allow-all / open, no denies | none (host network) | **no** — nothing to filter |
| `closed` / no network | empty netns only | **no** |
| loopback-only list | the floor expresses it natively | **no** |
| open with denies, any list carrying a deny row, or a list with a host/domain/cidr entry | empty netns | **yes** |

Consequences worth knowing:

- **One predicate, two consumers.** The same function answers for the launch
  path and for `PredictAccessEnforcement`, so an editor preview cannot promise a
  mechanism the launch will not run.
- **The disclosure names the mechanism that actually runs.** A policy that
  deploys a proxy reads `tclaude-layer bubblewrap + supervised loopback
  filtering proxy`; a loopback-only policy under the same authored engine keeps
  the native mechanism sentence, because that is what runs.
- **No idle proxy.** The proxy is started by the filtered launcher only on the
  discriminating branch, and its lifetime is the sandbox's. There is no daemon
  and no pre-warm.
- **Selecting `proxy` on a non-discriminating policy is not an error.** It is a
  latent choice, and the preview says so: the selection takes effect the moment
  a rule makes the policy discriminating.

## Protocol carriage — two carriages, one predicate

The single listener speaks both proxy protocols:

1. **HTTP proxy** — `CONNECT host:port` for all HTTPS (a tunnel, **not** MITM),
   and absolute-form plain HTTP (`GET http://host/… HTTP/1.1`), evaluated on the
   request-line host with default port 80.
2. **SOCKS5** (RFC 1928), `CONNECT` command only, no authentication method.
   `ATYP=DOMAINNAME` is a name target, exactly like an HTTP `CONNECT` host;
   `ATYP=IPV4`/`IPV6` is a literal target.

Both carriages parse a request into the same internal `(target kind, name or
address, port)` tuple and call the *same* evaluator (`sandboxproxy/target.go`,
`evaluator.go`). The carriage protocol never reaches the policy layer, so
deny-wins ordering, label-bound suffix matching, literal-only CIDR, and the
private-destination blocker are identical across the two by construction rather
than by parallel maintenance.

**No CA is installed, for any harness.** `CONNECT` is a tunnel, so there is no
TLS interception and nothing to trust: `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`,
Rust rustls trust stores, and Go's `x509` roots are all untouched. This removes
the largest single compatibility risk that proxy-based sandboxes usually carry.

**UDP ASSOCIATE is deliberately out of v1.** Carrying datagrams would mean the
floor had to relay them — new attack surface for a feature almost no harness
client uses — and it would reopen a resolver channel this posture deliberately
does not have. So **UDP and QUIC are blocked, not filtered, even for
SOCKS5-aware clients.** A client that negotiates UDP ASSOCIATE gets the RFC 1928
"command not supported" reply rather than an opaque failure, so a cooperating
client can fall back to TCP.

### What is blocked rather than filtered

Under this engine the floor denies, for every client:

- UDP and QUIC;
- ICMP and raw/packet sockets;
- **TCP from a proxy-unaware client** — any program that ignores
  `HTTP(S)_PROXY`/`ALL_PROXY` and opens a socket directly.

Those are denied, not filtered, and the disclosure says so in exactly those
words (`ProxyEngineCarriageNotice`). What SOCKS5 buys is that arbitrary
*proxy-aware* TCP is genuinely carried and filtered: a client that honors
`ALL_PROXY` reaches an authored `{"cidr": "10.20.0.0/16", "ports": [5432]}`
destination that a CONNECT-only proxy could not carry. Entries whose authored
ports are not obviously HTTP-ish therefore carry a per-entry caveat rather than
a bare "enforced" verdict.

`ssh` is the interesting middle case: it does not read `ALL_PROXY`, so plain
`ssh` fails closed. An operator-authored `ProxyCommand` can route it through the
SOCKS listener; tclaude does not configure that, and it is named here as an
available operator move rather than a promise.

## How the policy is evaluated

Policy is evaluated on the **requested target as the client stated it**, before
any resolution, identically for both carriages:

| Authored selector | Matches | Notes |
|---|---|---|
| `host` | an exact DNS name target | case/IDN-normalized to ASCII |
| `domain` (+`include_subdomains`) | label-bound suffix match on a name target | `badexample.com` never matches `example.com` |
| `cidr` | **IP-literal targets only** | see below |
| `loopback` | `127.0.0.1:P` / `::1:P` / `localhost:P` from the sandbox, connected to real host loopback | no synthetic address is involved |
| `ports` | the target port | an empty list means any port |

Deny is evaluated first and wins every overlap, so authoring order cannot change
a result.

A refused request gets a legible reason, which is a real advantage over a silent
packet drop: HTTP gets a `403` with a short capability-phrased body naming the
destination, and SOCKS5 gets reply code `0x02` (connection not allowed by
ruleset), which is the protocol's exact word for this.

### CIDR is deliberately literal-only

A name target is **never** resolved and then matched against CIDR rules.
Matching resolved addresses against CIDRs would silently hand every CIDR rule
the DNS-name authority the operator did not author — the same ruling the packet
posture makes when it refuses to let a CIDR rule authorize a DNS query. The
honest consequence is that `cidr` is rated **Partial** here, with a per-entry
disclosure saying so.

### Host-side resolution and the private-destination blocker

For an allowed name target the proxy resolves host-side and connects. The
resolved address is not re-checked against the authored list — there is no lease
model here, and name identity is the authority. It **is** checked against a
private-destination blocker **in allowlist modes** (baseline deny / list):
the connection is refused when the resolved address is loopback, link-local,
private (RFC 1918), CGNAT, ULA, or unspecified/multicast — unless the policy
authored a `loopback` rule or a CIDR covering that range, in which case the
operator asked for it.

Under an **open** baseline the reserved-space half of that blocker does not
apply: open means open, minus the authored denies, and applying it there would
narrow a policy the packet engine does not narrow. Two things still apply under
every baseline, and an operator debugging an open policy needs both:

- **Host loopback is never reached by default.** It is excluded from the open
  baseline's own default-accept, and the resolved-address check refuses it
  unless an authored `loopback` row covers the port. Under the packet engine an
  open posture puts the sandbox on the host network, where its loopback is its
  own — so honoring the default here would hand an open policy a destination it
  never had.
- **Deny rows are re-checked against the resolved address.** That is what
  actually closes rebinding onto a *denied* address for an open policy.

Two things follow. First, this closes by construction the DNS-rebinding route
the packet posture documents as an open gap: a permitted name whose answer is
attacker-controlled cannot be pointed at host services. Second — stated because
it is a real cost — an authored name that legitimately resolves into private
space stops working under this engine unless a `cidr` or `loopback` row covers
it, and the same profile works under the packet gateway. That narrowing is
disclosed per entry (`ProxyEngineNameSelectorDetail`), not left to be
discovered.

## Name authority, and the boundary of that guarantee

The proxy can only decide on names because nothing inside the sandbox can turn a
name into a literal on its own. The floor synthesizes a loopback-only
`/etc/hosts` (`ProxyNetworkHostsFile`) and drops every host-derived mapping: a
mapped name would otherwise become an address literal with no query leaving the
namespace, and a literal is matched against CIDR rows only — so an authored deny
on that name would have nothing to match.

The resolver configuration itself is left alone, because in an empty namespace a
query has nowhere to go: the socket-backed NSS modules live under `/run`, which
the constructed root does not bind, and a `resolv.conf` naming a loopback stub
can only reach the sandbox's own loopback, where the floor grants no
capabilities and no namespace root, so nothing can bind port 53 to answer.

The guarantee has **two named boundaries**, and both are stated rather than
claimed away:

1. The sandbox inherits the host's `/etc/nsswitch.conf` and NSS modules, so
   handing a resolver socket back to the sandbox restores exactly the
   name-to-literal conversion the hosts file prevents. Two authored axes can do
   that, and **both are refused** at the capability surface over one shared list
   of known resolver paths — `NetworkEngineResolverSocketConflict` on the
   `unix_sockets` axis, and `NetworkEngineResolverFilesystemConflict` on the
   filesystem axis (a read-only bind does not stop `connect(2)` on a socket).
2. A resolver that list does not know — a private NSS module over a socket an
   operator builds themselves — is outside it. The list refuses the resolvers a
   real host ships; it is not a proof of exhaustiveness.

The delivered property is therefore "no automatic name-to-address conversion",
not "no host-derived address knowledge". `/etc/resolv.conf` and friends stay
readable and still disclose host addresses. Disclosure is not authorization: a
literal the sandbox learns still has to pass the proxy's CIDR rows.

## The proxy environment is tclaude's

On a proxy-engine launch the launcher **replaces** all eight proxy variables
inside the namespace, and it does so *after* the model-transport gate has run
(`proxyNetworkSandboxEnv`):

```text
HTTP_PROXY  = http://127.0.0.1:<port>     http_proxy  = …
HTTPS_PROXY = http://127.0.0.1:<port>     https_proxy = …
ALL_PROXY   = socks5h://127.0.0.1:<port>  all_proxy   = …
NO_PROXY    = ""                          no_proxy    = ""
```

Four details are load-bearing:

- **The port is ephemeral and chosen by the in-namespace bind.** A fixed port
  would collide with dev servers the harness itself starts, and nothing on the
  host can squat a port that is bound from inside.
- **`socks5h`, not `socks5`.** The `h` keeps name resolution at the proxy, where
  the authored host and domain rows are evaluated. A client forced to plain
  `socks5://` resolves locally, finds no resolver, and fails — which is the
  intended fail-closed behavior, not a filtering gap.
- **A foreign proxy variable still refuses the launch.** The model-transport
  resolver inspects the pre-injection environment (host environment plus
  authored `EnvironmentEntry` overrides) and refuses any non-empty
  `HTTP(S)_PROXY`/`ALL_PROXY` there, because a proxy hides the real destination
  from the endpoint resolver. This is **ordering, not recognition**: tclaude's
  own values are never in the inspected set, and they are never allowlisted by
  value. A foreign variable that happens to name a loopback address refuses
  exactly like any other, so setting the environment cannot dress a foreign
  proxy up as ours. The Claude live-reloaded-settings path keeps refusing too,
  because Claude re-reads `settings.json` env while a session runs and a
  one-time preflight cannot freeze it.
- **`NO_PROXY`/`no_proxy` are overridden to empty, with a disclosure.** They are
  set to empty rather than removed, because empty exempts nothing while absent
  lets a harness fall back to its own default exemption list (which usually
  includes localhost and private space). An inherited exemption can only carve
  destinations *out* of the only route that exists, so those attempts hit the
  floor and fail closed — a usability problem rather than a security one, and
  refusing a launch over it would be disproportionate. When the host actually
  carried a non-empty value, the launch records an access notice saying it was
  overridden and that the exempted destination must be authored as a network
  rule if the sandbox needs it.

The tclaude proxy also does **not** read ambient proxy environment for its own
upstream. Upstream chaining is off, and would have to be explicitly authored.

## Gated launch sequence

The harness does not start before the proxy is accepting:

```text
resolve and validate profile
          │
          ▼
compile the proxy policy through the evaluator
          │
          ▼
bubblewrap creates private namespaces (empty netns)
          │
          ▼
trusted in-namespace bootstrap binds 127.0.0.1:<ephemeral ≥1024>
          │
          ▼
listening descriptor is passed OUT over the launch-private socket
          │
          ▼
host-side proxy accepts on that namespace-owned descriptor
          │
          ▼
bootstrap reads the readiness token, unlinks itself, execs the harness
```

The direction is the point, and it is why this is not a port forward. The
listening socket is created *inside* the namespace by a bootstrap that holds no
capabilities, on an unprivileged port; only the descriptor crosses out, over the
same launch-private Unix packet socket and the same peer-credential and
`/proc`-netns verification the DNS broker already uses. The host never binds
anything into the sandbox, and the sandbox never receives a route out of it.

Compared with the packet gateway there is no nft policy, no resolver files, no
`pasta`, and no capability arguments. The whole in-namespace footprint is three
things: one sealed executable, which the bootstrap unlinks before exec; one
bind-mounted readiness socket, whose host end is closed and unlinked after the
single handoff it exists for; and the sealed loopback-only `/etc/hosts` that the
name-authority guarantee above rests on, bound read-only over the host's.

Sandbox-private loopback stays usable for the harness's own servers. A sandboxed
process can connect to its own listeners; it cannot reach the host's loopback
except through an authored `loopback` rule carried by the proxy.

## Prerequisites and trust checks

Only two, and this is the engine's headline operational advantage:

- bubblewrap can create the mount, network, and PID namespaces this floor needs;
- Linux pidfds are available.

There is no `pasta`, no `nft`, no trust walk over their paths, no
`CAP_NET_ADMIN` or `CAP_NET_BIND_SERVICE` probe, and no port-53 broker. **This
engine works on hosts where the packet gateway cannot run** — without being a
fallback for them.

The failure policy differs from the packet gateway's in the outcome, not just in
the checks: **a host that cannot build this floor refuses the launch** rather
than starting it with the rules unenforced (`ProxyEngineLaunchCondition`). The
one place a proxy-engine profile still widens to open is a target whose
capability cells are not activated yet — see
[Harness cooperation and activation](#harness-cooperation-and-activation) — and
that widening is disclosed with its own notice.

## Supervision and failure behavior

The outer tclaude relay supervises bubblewrap and the proxy:

- the harness cannot start before the proxy is accepting on the handed-out
  descriptor;
- if the proxy exits, the relay kills the sandbox;
- when the sandbox exits, the relay terminates the proxy;
- bubblewrap keeps `--die-with-parent`, and the child is pinned with a pidfd,
  avoiding PID-reuse races.

A proxy failure is a sandbox failure. There is no state in which the harness runs
with the proxy gone — and because the floor is an empty netns, even that state
would be fail-*closed* rather than open. Every component's failure mode here is
"no network".

The running proxy records one decision per connection at `debug` level
(`sandbox filtering proxy decision`). What it logs is a closed set by
construction — carriage, target kind, host or address, port, verdict, and the
matched rule's index, selector and authored value — because the record is built
from the evaluated target and the matched rule, neither of which carries a
request line, a path, a query, or a header. That is a
security property rather than tidiness: a `Proxy-Authorization` header or
userinfo in an absolute-form URL cannot reach the log.

## Harness cooperation and activation

Capability cells for this engine are activated **per harness, platform, and
sandbox implementation**, and a cell flips only in the PR that carries its green
named CI smoke. Cells follow the activation record
(`proxyEngineActivatedSmokes`), never a proposal and never a static scan of a
binary.

Currently activated: **Claude Code 2.1.220, Codex 0.145.0 and OpenCode 1.18.6,
on Linux.** The two plain-CLI harnesses are backed by
`TestPinnedProxyHarnessCooperation` and `TestPinnedProxyToolEgress`; Codex was
activated one milestone after Claude Code, from the same runs rather than from
new ones — the evidence was green from the first shard run, and what was missing
was the record, which is the rule working rather than an exception to it.

OpenCode (TCL-891) is backed by `TestOpenCodeProxyFloorCooperation`
(smoke flow `40-opencode-floor`, green named run `30654121316`) plus the shared
floor row `TestPinnedProxyToolEgress`. Unlike Codex, it proves something new:
OpenCode launches through the agentd-owned Unix-relay server boundary rather
than as a plain CLI, and that boundary **refused this engine outright** until
TCL-891 generalized the inherited-descriptor contract from the packet
supervisor's fd layout to both engines. Its row therefore rests on a smoke that
had no way to exist before, not on a second reading of the plain-CLI runs.

Two facts about that row, and they have **different subjects** — conflating them
is the easiest mistake to make here:

- **The floor** refuses undeclared destinations (`not_authorized`) and deny-row
  destinations (`denied_by_rule`) over **both carriages**, and carries an
  authorized destination over both, all observed executing in one launch by an
  in-namespace cooperating client.
- **OpenCode itself** carries model traffic over **HTTP CONNECT only** and
  ignores `ALL_PROXY`, measured under one-carriage isolation in
  `TestOpenCodeProxyCarriageCooperation` (flow `30-opencode-carriage`) and
  reproduced behind the real floor.

The consequence is a **per-harness disclosure, not a rating change**: a
destination OpenCode would need the SOCKS5 carriage for has no carriage and no
route out of the empty namespace, so it is **blocked by the floor rather than
filtered by the policy** (`ProxyEngineOpenCodeCarriageNotice`). The selector
cells are unchanged — host, domain, cidr, port and loopback are all expressible
over HTTP CONNECT, and the cells rate what the evaluator enforces. The two
plain-CLI harnesses do not carry that sentence because `TestPinnedProxyToolEgress`
records their ordinary tool traffic carrying over both carriages; no equivalent
measurement exists for OpenCode's tools, and the sentence deliberately does not
claim one.

With every registered harness now listed on Linux, the activation rule's
"a harness with no record stays unenforced" case has no registered subject left.
It is kept under test at the level where it is still real — the record lookup's
fail-closed default in `TestProxyEngineActivationIsScopedToItsEvidence` — rather
than through a Darwin row, whose cells are unenforced for a reason that dominates
that lookup and would advertise coverage the rule does not have.

What the first activation run (on-main run `30609001363`) actually showed, as
distinct from what was hypothesized:

- both pinned harnesses reach their model origins through the proxy floor over
  **HTTP CONNECT only** — `api.anthropic.com` for Claude Code 2.1.220,
  `api.openai.com` for Codex 0.145.0;
- **neither uses SOCKS on the model path**, even though `ALL_PROXY` appears in
  both binaries. Bundled proxy code proves only that it is bundled; this is
  exactly why cells never flip on a static scan;
- tool-egress markers behaved as expected: `curl` over both carriages,
  `git`-over-HTTPS, and Go module fetches all carried.

Two structural facts hold regardless of harness:

- **Tool egress is inherently partial.** A harness's shell tool runs arbitrary
  programs. `curl`, `git`-over-HTTPS, `go`, `npm`, and `pip` honor proxy
  environment; `nc`, DNS clients, `ssh`, and anything using raw sockets do not.
  Under this engine every non-honoring client fails closed — denied, not leaked
  — and the cell wording says "blocked" for them, never "filtered".
- **MCP stdio servers** are arbitrary programs with the same story; the
  `MCPBypass` marker on the capability row stays honest here.

## Refusal catalogue

Launches this engine refuses outright, rather than starting with rules
unenforced:

| Refusal | Why | Where |
|---|---|---|
| A resolver socket on the `unix_sockets` axis | restores in-sandbox name-to-literal conversion and defeats name authority | recorded on the capability row, refused at the ladder's socket rung (`SocketAllowlist`) |
| A filesystem grant covering a resolver socket | the same authority taken away through the filesystem; a read-only bind does not stop `connect(2)` | returns from the capability seam (`NetworkAllowlist`), Linux only |
| The floor's prerequisites are unavailable | this engine refuses rather than launching unfiltered | spawn guard and `ResolveTclaudeLayerForEngine` |
| A policy that does not compile | a policy the running evaluator could not answer from must never reach the relay | `sandbox_bwrap.go`, `proxy_network_bridge_linux.go` |
| A foreign `HTTP(S)_PROXY`/`ALL_PROXY` in the launch environment | the real destination sits behind a proxy the endpoint resolver cannot resolve | model-transport gate |
| The same variables authored in Claude's `settings.json` (Claude Code only) | Claude re-reads settings env while a session runs, so a one-time preflight cannot freeze the route | `claudeSettingsProviderVariable` |
| A provider endpoint using the packet gateway's synthetic host-loopback name | only the packet engine installs that mapping; under this engine it resolves to nothing, and the remedy is `localhost`/`127.0.0.1`/`::1` plus a `loopback` rule | `validateModelTransportLoopbackForPlatform`, `sandbox_bwrap.go` |
| Packet and proxy engines in one launch | one launch runs one engine; deciding which policy is authoritative later is the shape of question that produces a bypass | `sandbox_bwrap_winch_relay_linux.go` |
| A stacked relay binding combined with the proxy engine | the same rule, for the nested-harness relay | `sandbox_bwrap_winch_relay_linux.go` |

The two resolver refusals are deliberately asymmetric in where they refuse and
which capability kind they carry — each kind names the axis whose authored rows
the operator must edit, and the filesystem one has no ladder rung to defer to.
The reasoning is recorded at the socket conflict site in
`pkg/claude/harness/access_enforcement.go`.

## Security properties and limits

The boundary provides:

- an empty network namespace as the floor: no route exists that the proxy does
  not serve;
- destination authority on the **name the client requests**, before resolution,
  with no DNS-lease window and no shared-IP authority;
- a host-side private-destination blocker in allowlist modes, which closes the
  DNS-rebinding route by construction;
- no synthetic host-loopback address, and therefore none of the packet posture's
  synthetic-address reservation gap: the `loopback` selector is the sole
  host-loopback authority here;
- no CA installation and no TLS interception anywhere;
- no `CAP_NET_ADMIN`, no `CAP_NET_BIND_SERVICE`, no userns-root mapping, and no
  privileged in-namespace process;
- no hidden model-transport bypass: the complete harness process is inside the
  same namespace;
- a per-connection decision record whose fields cannot carry credentials.

Its deliberate limits are:

- UDP, QUIC, ICMP, and raw sockets have no route out — blocked, never filtered;
- a proxy-unaware TCP client is blocked, not filtered;
- a `cidr` rule matches only a target the client states as a literal; a name
  that resolves into the range is not admitted by it;
- an authored name that resolves into private or reserved space is refused in
  allowlist modes unless a `cidr` or `loopback` row covers it, so a profile that
  works under the packet gateway can stop working here;
- a private NSS module the known-resolver list does not cover is outside the
  name-authority guarantee;
- capability cells are per harness and only activated by a named green smoke; an
  unactivated target widens to open with a notice.

### Capability ratings

Allow selectors, under a discriminating policy with this engine:

| Selector | Rating | Why |
|---|---|---|
| `host`, `domain` | **Full** | decided on the requested name, before resolution — no lease window, no shared-IP authority. Narrowed, and disclosed as narrowed, by the private-destination blocker. |
| `cidr` | **Partial** | literal targets only (see above) |
| `loopback` | **Full** | reached through the proxy on the authored ports; no synthetic address can substitute for the rule |
| ports | **Full** | the port is part of the requested target |

Note the mirror image of the packet gateway, which is the honest headline:
host and domain **lose** their TTL/shared-IP caveat and become genuinely Full,
while CIDR **drops** from Full to Partial.

Deny selectors:

| Selector | Rating | Why |
|---|---|---|
| `host`, `domain` | **Partial** | The proxy decides on the identity the *client* states, and a client can state an IP literal instead of a name. Literal targets are matched against `cidr` rows only, and there is no TLS interception to recover the name, so a name deny is bypassable by connecting to the denied host's address directly. Rated Partial **unconditionally**: whether such a literal is reachable depends on the whole rule set, and a rating that flipped as unrelated CIDR rows were edited could not be reasoned about. The remedy is named per entry — add a `cidr` deny for the addresses that name resolves to. |
| `cidr` | **Partial** | the exact mirror: a *name* resolving into a denied range is not matched |
| `loopback` | **Full** | Legitimately Full, and it is not an oversight that it sits between two Partials. The escape that makes a name deny Partial is stating an address instead of a name — and for loopback there is no such escape, because the evaluator folds every spelling of loopback into **one identity** before matching: a literal loopback target is matched against the loopback name, so `localhost`, `127.0.0.1`, `::1` — and the unspecified spellings that also reach the host — all answer to the same row. There is no literal that slips past a loopback deny. Do not "fix" this to Partial for symmetry; it would be a false rating. |
| deny ports | **Full** | the port is part of the requested target |

For the operator-facing summary, model-endpoint gating, and the wider
filesystem/socket boundary, see
[Isolated-with-agentd network posture](sandboxing.md#isolated-with-agentd-network-posture).

## Code map

The main implementation seams are:

| Area | Source |
|---|---|
| Profile schema, validation, and intersection | `pkg/claude/common/sandboxpolicy/access_rules.go` |
| Engine field, composition, and the discriminating predicate | `pkg/claude/common/sandboxpolicy/network_engine.go` |
| Resolver-socket and resolver-filesystem conflicts, known resolver list | `pkg/claude/common/sandboxpolicy/network_engine_sockets.go` |
| Synthesized loopback-only `/etc/hosts` | `pkg/claude/common/sandboxpolicy/filtered_network_nft.go` |
| Target parsing for both carriages | `pkg/claude/sandboxproxy/target.go` |
| HTTP `CONNECT` and absolute-form handling | `pkg/claude/sandboxproxy/http.go` |
| SOCKS5 handling | `pkg/claude/sandboxproxy/socks5.go` |
| Policy evaluation and the private-destination blocker | `pkg/claude/sandboxproxy/evaluator.go` |
| Listener, accept loop, and host-side dial | `pkg/claude/sandboxproxy/server.go`, `dial.go` |
| Bootstrap, descriptor handoff, proxy environment, decision log | `pkg/claude/session/proxy_network_bridge_linux.go` |
| The proxy environment the launcher owns, and the NO_PROXY disclosure | `pkg/claude/session/proxy_network_env.go` |
| Floor mapping, engine predicate, and launch refusals | `pkg/claude/session/sandbox_bwrap.go` |
| Model-transport gate (pre-injection environment) | `pkg/claude/session/model_transport_launch.go` |
| Supervision and fail-closed teardown | `pkg/claude/session/sandbox_bwrap_winch_relay_linux.go` |
| Capability cells, activation record, and disclosure strings | `pkg/claude/harness/network_engine_disclosure.go`, `access_enforcement.go` |
