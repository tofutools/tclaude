# Filtered-proxy smokes

The real-boundary smokes for the `network.engine: proxy` posture, and the
evidence discipline that decides whether their results may back a capability
cell.

**Never run these locally.** The flows build real bubblewrap sandboxes, create
network namespaces with `sudo`, and temporarily rewrite `/etc/hosts`. CI is the
only place they belong, and `run.sh` enforces that: outside CI it refuses unless
`TCLAUDE_ALLOW_LOCAL_PROXY_SMOKE=1` is set, and it rejects any argument it does
not recognise rather than falling through to the destructive default. The two
safe entry points are:

```bash
scripts/filtered-proxy-smoke/selftest.sh              # proves the evidence checker
scripts/filtered-proxy-smoke/run.sh --validate-only   # + manifest/flow consistency
```

`bash` 4 or newer is required (associative arrays, `mapfile`); the script runs
only on the Linux CI shard.

Both are also run by `go test ./pkg/claude/session -run TestProxySmoke`, so
ordinary CI catches a broken guard without waiting for the smoke shard — and
they run for **every** smoke shard's manifest, not only this one.

## What is shared and what is this shard's own

The evidence checker, its self-test, the manifest drift guards, the flow runner
and the network fixtures live in `scripts/lib/smoke/` and are shared with every
other smoke shard (`scripts/proxy-posture-e2e/` is the other one today). The
discipline is therefore defined once, self-tested once, and cannot differ
between shards — a forked copy would be a second place the rule "a skipped,
renamed or zero-test smoke is a hard failure" could quietly weaken.

What stays in this directory is what is specific to these smokes: the host
packages they need (`lib/prereqs.sh`), the harness pins (`lib/harnesses.sh`),
the flows, and the manifest that says which evidence they must produce.

## Why this lives here and not in a workflow

Everything that decides *what runs*, *what counts as evidence*, *which harness
versions are pinned* and *how fixtures are built* is in this directory and the
shared lib. The CI job is generic: check out, set up Go and Node, invoke
`run.sh`.

That split exists because `.github/workflows/**` needs an operator with
`workflow` scope to merge. With the logic inline, every new smoke, every
fixture tweak and every pin bump cost an operator merge. Now they cost a repo
edit and a review.

## Host prerequisites

`lib/prereqs.sh` installs `bubblewrap`, `socat` and `iproute2`. That list is
deliberately **shorter** than the packet gateway's sibling smoke, which also
installs nftables and builds pasta from source: the proxy floor reaches its
namespace through bubblewrap's plain unshare and calls neither. Installing them
here would quietly undermine the claim that this floor does not need them.

That claim belongs to **this** shard. `scripts/lib/smoke/packet-floor.sh` does
provision pasta and nft, and the posture-e2e shard uses it — because its
loopback-only scenario is a filtered policy that deploys *no* engine and
therefore runs the packet floor. Sharing the code does not share the claim: what
matters here is that these flows never call it.

Needing a new tool is a repo edit here, not a workflow merge — the same reason
the flows and the harness pins live in this directory.

## Adding a smoke

1. Add `flows/<NN>-<name>.sh` defining `flow::run` (and optionally
   `flow::describe` for the failure summary, `flow::report` for an
   operator-facing extract on success).
2. Add its required top-level test names to `manifest.txt`.

Nothing under `.github/` changes. `run.sh` discovers flows in sorted order and
runs each in a subshell, so a flow cannot leak state into the next.

## The evidence discipline

`go test` exits 0 for a skipped test, for a `-run` filter that matches nothing,
and for a package with no test files. **An exit status is not evidence.**
`lib/evidence.sh` is, and it refuses:

| shape | why it is not evidence |
| -- | -- |
| `--- SKIP:` | a gated-out smoke proves nothing |
| name absent | renamed, removed, filtered out, or build-tagged away |
| `no tests to run` | the filter matched nothing |
| empty log | the flow died before testing anything |
| subtest pass only | an indented pass does not speak for its parent |
| prefix match | `TestSmoke` is not satisfied by `TestSmokeExtra` |
| no names declared | a flow requiring nothing could not fail |

One shape it does **not** catch, stated rather than implied: a parent test
reporting a top-level pass while every one of its subtests skipped. No current
smoke has that shape, but nothing here would notice if one were introduced —
like a rename applied consistently to both the test and the manifest, it is a
review question.

`run.sh` proves all of that against synthetic logs **before** it runs anything,
so a checker that has rotted fails the run instead of passing every flow.

It also refuses manifest drift in both directions: a flow with no manifest
entry (a smoke that cannot fail) and a manifest entry with no flow (evidence
claimed for a smoke that no longer runs).

## The trade-off, stated plainly

This discipline now lives in files agents can edit, where it used to live in a
workflow only an operator could change. That was the price of making the smokes
extensible without a workflow merge each time.

The guard from here on is the manifest plus cold review. Deleting a line from
`manifest.txt` is a visible, reviewable diff that says exactly which evidence is
being dropped, and the consistency checks make a flow-without-evidence
impossible. What no mechanism here can catch is a rename applied consistently to
both the test and the manifest — that is a review question, not a mechanical
one, and it is the reason cell flips still require an independent reviewer.
