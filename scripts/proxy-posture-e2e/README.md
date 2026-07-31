# End-to-end proxy-posture smokes (M2.6)

One dedicated, visible verification that the **assembled** posture works:
authored policy in, real launch, real enforcement observed — for each of the
four postures the conditional-deployment table distinguishes.

The per-milestone smokes each prove their own piece (the floor, the policy
engine, harness cooperation, tool egress) at the time it lands. None of them
proves that the pieces together behave as authored, and in particular none of
them proves the harder half: that the three postures which must deploy **no**
proxy do not deploy one. That is this shard.

**Never run these locally.** The flows build real bubblewrap sandboxes, create
network namespaces with `sudo`, and temporarily rewrite `/etc/hosts`. CI is the
only place they belong, and `run.sh` enforces it. The two safe entry points are:

```bash
scripts/proxy-posture-e2e/selftest.sh              # proves the evidence checker
scripts/proxy-posture-e2e/run.sh --validate-only   # + manifest/flow consistency
```

Both are also run by `go test ./pkg/claude/session -run TestProxySmoke`, for
this shard and every other, so ordinary CI catches a broken guard without
waiting for a smoke shard.

## The four scenarios

| flow | authored policy | must be observed |
| -- | -- | -- |
| `10-discriminating` | list + host/CIDR allows + a deny beating an overlap | proxy deployed; allow carried over **both** carriages; deny refused with `denied_by_rule` at the proxy; direct TCP, UDP and ICMP refused by the floor |
| `20-open-deny` | open baseline + one deny row | proxy deployed; deny enforced; private space (RFC1918 name, RFC1918 literal, reserved literal) still **reachable** per the amended §4.4 ruling; host loopback still refused without an authored loopback row |
| `30-loopback-only` | loopback-only list | **no** proxy: no process, no listener in the sandbox namespace beyond the packet floor's own DNS broker, no injected discovery, no decision record — while the authored loopback port is reachable and an unauthored one is not |
| `40-allow-all` | open, no denies | **no** floor and no proxy: the fixture is reachable directly |

Every scenario authors `engine: proxy`. The four differ only in the policy,
which is the variable the conditional-deployment ruling turns on — so a
difference in outcome cannot be attributed to anything else.

## How absence is proven

"No proxy is deployed" is the claim most easily faked by a test that simply
never looked. Four independent observations back it, and the first two also run
in the scenarios that DO deploy a proxy, where they must find one:

- a host-side watch of the process table for the whole life of the sandbox,
  matching `argv[0]` against this shard's own `tclaude` binary;
- a complete inventory of the listening sockets in the sandbox's **own** network
  namespace, which is where a deployed proxy's listener lives — exact rather than
  "no proxy port", because a sandbox that was never told about a proxy cannot
  know which ephemeral port to exclude;
- the proxy discovery variables the launcher injects, which must be absent;
- the proxy's own decision record, which must not exist.

Whether a launch deploys a proxy is asked once, of `TclaudeLayerDeploysProxy`
over the engine `DeployedNetworkEngineForRules` resolves — the same predicate
the launcher uses. Every assertion compares against that answer rather than
against a per-test expectation, so a scenario cannot be written to expect a
deployment the launcher would not perform.

## Preview/launch parity

Each scenario also asks the preview surface, in the same run, what it would tell
an operator — and requires it to name the mechanism that actually ran. A
prediction of "filtering proxy" for a launch that deployed none (or the reverse)
fails here rather than in front of an operator. The platform is passed
explicitly rather than read from `runtime.GOOS`, so the M3 Darwin arm is an
added platform row rather than a rewrite.

## Host prerequisites, and why one flow installs its own

`lib/prereqs.sh` installs `bubblewrap`, `socat` and `iproute2`. Three of the
four scenarios build the proxy engine's floor, which calls neither `pasta` nor
`nft`.

The loopback-only scenario is the exception: a loopback-only list is a filtered
posture that is not discriminating, so it deploys no engine and the launch runs
the **packet** floor. That flow provisions `pasta` and `nft` itself, through the
shared `scripts/lib/smoke/packet-floor.sh`, for two reasons — an upstream passt
build break must not fail the three scenarios that never touch that floor, and
it must never read as a posture regression. Every failure from that helper is
reported as a *prerequisite* verdict with its own step-summary block saying that
no policy was evaluated.

That helper is deliberately free of workflow-specific assumptions, so
`ci.yml`'s sandbox-v2 job — which carries an inline copy of the same pin today —
can later delegate to it instead of maintaining a second copy.

## What is shared

The evidence checker, its self-test, the manifest drift guards, the flow runner,
the network fixtures and the userns unlock live in `scripts/lib/smoke/` and are
shared with `scripts/filtered-proxy-smoke/`. The discipline is defined once and
self-tested once; a forked copy would be a second place the rule "a skipped,
renamed or zero-test smoke is a hard failure" could quietly weaken.

## Adding a scenario

1. Add `flows/<NN>-<name>.sh` defining `flow::run` (and optionally
   `flow::describe`), building its fixture with `posture::fixture_up <index>`.
2. Add its top-level test name to `manifest.txt`.

Nothing under `.github/` changes. The CI job is generic: check out, set up Go,
invoke `run.sh`.
