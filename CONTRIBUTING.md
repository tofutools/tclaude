# Contributing

## Quick start

```bash
git clone https://github.com/tofutools/tclaude
cd tclaude
go install . ./cmd/...
```

The repo builds three binaries: the `tclaude` CLI at the module root, and
under `cmd/` the standalone `tclaude-agentd` daemon and the `tclaude-hub`
federation relay (see `docs/federation.md`). A bare `go install .` builds
only the root package, so name both patterns — `./cmd/...` matches the
daemon and the hub. Do not widen it to `go install ./...`: that would also drop the
non-production probe under `scripts/` into your `GOBIN`.

The daemon is built into the CLI as `tclaude agentd serve`, so the second
binary matters mainly when you are working on it or running a daemon-only host.
Installing the pair keeps them from drifting apart across rebuilds.

## Running tests

```bash
go build ./...
go test ./...
golangci-lint run ./...
```

Platform sandbox assumptions are separate hardware-backed hard gates; the
contract lives in `pkg/claude/sandboxassumptions`. On a compatible Linux
host:

```bash
TCLAUDE_SANDBOX_ASSUMPTIONS=1 \
  go test ./pkg/claude/sandboxassumptions \
    -run '^TestBubblewrapAssumptions$' -count=1 -v -timeout=180s
```

### What PR CI actually runs

`go test ./...` locally runs everything. PR CI does not: each shard is graded
against the pull request's base commit by `scripts/ci/affected`, which walks
the real import graph — including test-only imports — and runs only the
packages a change can reach. The step log always names the verdict and why it
was reached.

The selector is deliberately conservative. A changed file it cannot attribute
to a Go package — `go.mod`, a workflow, a shell script — falls back to running
the whole shard, as does a change to the selector itself or setting
`TCLAUDE_CI_FULL_TESTS=1`. Documentation (`docs/`, `mkdocs.yml`, root-level
Markdown) is the exception: it feeds no build, so a changed doc selects only
the packages whose tests declare reading it with a `//ci:reads docs/x.md`
comment line (or a `docs/dir/` prefix) — in a Go test file or a `.test.mjs`
suite, which counts for the Go package that runs it — and nothing at all
otherwise. A PR
that changes only docs skips the Go jobs entirely. Release and
manual-release always run every shard in full, so the merge commit is measured
without selection before anything ships.

A test that reads a file outside its own package directory without importing
that package is invisible to the graph. Keep such reads inside the package, or
reach the file through an import, as `pkg/claude/agentd/jstest` does. When a
test genuinely has to scan the whole module — the structural guards that parse
every production file, for instance — no import graph can predict it, so put a
`//ci:whole-tree` comment in the file. That opts its package out of selection
entirely: every change selects it. `scripts/ci/affected` has a guard test that
fails when a module-root scanner is missing the marker, so this is enforced
rather than remembered. A second guard fails when a test climbs out of its
package and names the docs tree without a `//ci:reads` directive.

Two more things worth knowing. A green PR check means "everything this change
can reach passed", not "the whole suite ran". And running the selector by hand
against your working tree under-reports untracked files, since it grades a
`git diff`; commit first, or pass `-head`.

### Filesystem paths in tests

A temporary directory's spelling is not its identity. macOS commonly returns
`/var/folders/...` while Git and filesystem canonicalization return
`/private/var/folders/...`. `filepath.Clean` and `filepath.Abs` do not resolve
symlinks.

Use `testutil.CanonicalTempDir(t)` from `pkg/testutil` for fixtures that need a
canonical path: Git identity comparisons, resolved sandbox grants, and
no-follow filesystem operations. The helper retains `t.TempDir` cleanup and
has no application dependencies, so low-level packages can import it too.
Migrate existing fixtures when touching them; ordinary temporary files do not
need a repository-wide rewrite.

When testing a public API that accepts paths, explicitly include a symlinked
input and check the intended contract: preserve the supplied spelling, resolve
it, or reject it. Do not canonicalize those inputs merely to make assertions
pass, and do not weaken production no-follow checks to accommodate a fixture.

Run focused tests through the shared wrapper:

```bash
scripts/test.sh ./pkg/claude/agentd -run '^TestMaterializeAgentDirectories' -count=1
```

It forwards all arguments to `go test` (defaults to `./...`), supplies a fresh
symlinked `TMPDIR`, and removes its scratch directory after the command exits.
Tests and subprocesses therefore see the alias even on Linux. The shared CI
action uses this wrapper for Linux shards; macOS retains its native temp
layout. A fresh `TMPDIR` can invalidate cached test results for tests that read
it. The wrapper does not simulate other macOS behavior or replace macOS CI.

### Deterministic timing in tests

Make correctness conditions causal. Use a channel, barrier, callback, wait
group, or observable state to prove that work reached the relevant point before
allowing it to continue. Do not sleep to “give a goroutine time,” and do not
prove that an action was suppressed by observing nothing for a few
milliseconds. For example, a queued-writer test should wait until the writer is
actually contending on the lock through an explicit hook or channel barrier,
then assert state before releasing it.

Use the narrowest timing mechanism that matches the boundary:

1. Prefer causal handshakes or observable state when ordering is the contract.
2. Use Go's `testing/synctest` when all relevant goroutines, channels, timers,
   tickers, and contexts can live and durably quiesce inside one bubble.
3. Inject a clock, timer, or sleeper when production behavior is genuinely
   time-driven and the system cannot fit in a synctest bubble.
4. At real filesystem, subprocess, tmux, network, or other OS boundaries, poll
   production-observable state with a generous failure deadline and useful
   diagnostics, or add a controlled helper-process handshake.
5. Assert real elapsed time only when wall-clock performance is itself the
   contract.

`testing/synctest` does not virtualize external I/O, and `synctest.Wait` does not
treat mutex contention or syscalls as durable blocking. Use explicit hooks or
channel barriers for mutex/OS-lock contention. Real subprocesses, fsnotify
watcher descriptors, and database or daemon goroutines created outside the
bubble usually need a handshake, injected-time seam, or observable-state
polling instead. Pure debounce or state-machine logic fed by bubbled channels
can still be a good synctest candidate. Do not distort production boundaries
merely to force them into a bubble.

A generous deadline that only prevents a hung test is a liveness guard, not a
correctness assertion, and is acceptable. Keep it well outside normal
operation, make timeout failures diagnostic, and never replace a flake by
broadly increasing sleeps or retrying the whole test. A deadline created inside
a synctest bubble advances in fake time and tests controlled behavior; it is not
a wall-clock hang guard. Rely on the outer `go test -timeout` watchdog for a
stuck bubble. Preserve shared test harnesses and parallelism; deterministic
conversions should keep total suite time flat or make it faster.

## Dashboard frontend

### Vendored dependencies

The dashboard uses browser-native ES modules. Preact islands use HTM for
component templates, so editing or running the dashboard requires no Node
install, compiler, or frontend build step: the normal `go install` workflow
embeds everything it needs. Runtime modules are pinned and committed under
`pkg/claude/agentd/dashboard/third_party/preact/`; the dashboard never loads them
from a CDN.

Dependency upgrades are deliberately rare and reviewed as vendored-code
changes. Update the exact versions, hashes, source maps, and license metadata
in that directory's `README.md` together, then run the ordinary Go test suite.

### Component tests

Component tests live in `pkg/claude/agentd/jstest/*.test.mjs` and use Node's
built-in `node:test` runner plus `createPreactHarness` from
`jstest/preact-harness.mjs`. The harness loads the real dashboard import map,
exact vendored Preact modules, HTM, Signals, Preact test utilities, and a
committed LinkeDOM runtime. It provides `mount`, `act`, `input`, `fireEvent`,
`getByRole`, `getByLabelText`, and `importDashboardModule` helpers.

Prefer behavioural assertions through accessible roles and labels. Dispatch
real DOM events, use stable keys, and unmount every tested component so effect
cleanup is observable. Run a focused file directly with:

```bash
node --test pkg/claude/agentd/jstest/your-component.test.mjs
```

The ordinary `go test ./...` command also runs every `*.test.mjs` file. CI has
Node installed and fails loudly if Node or a committed test-runtime dependency
is missing. No npm install, browser, CDN, or network access is needed.

### State and DOM ownership

`snapshot-poll.js` owns the dashboard's single 2-second snapshot schedule;
`refresh.js` owns the request/render/publish transaction. Accepted server state
flows through `snapshot-store.js` and bounded feature-state adapters. Feature
components read their state there and route refreshes, retries, and mutations
through explicit action modules. Islands must not import `refresh.js`, start
competing poll timers, or infer application state from DOM nodes.

Every island owns reconciliation of the hosts declared by its loader.
Non-component modules must not independently replace a claimed host or treat
its DOM as durable application state. Narrow adapters are allowed only when the
island explicitly exposes the boundary: delegated actions may read an event
target's `data-*`; transient editors may hide a managed node and insert a
sibling; and the native `SlopMachine` component mounts its reel tree into an
explicit opaque child host that `slop-fx.js` may temporarily replace. Keep those
constraints covered behaviorally and do not widen them. Code outside claimed
hosts may remain imperative where it has a clear owner.

The global Signals boundary is for server-backed snapshot data, connection and
poll metadata, the active tab, and computed feature views. Ephemeral UI state
such as focus, hover, an open disclosure, draft form text, or a local selection
belongs in the owning component. Persisted preferences remain server-backed:
write them through an explicit action and let the authoritative snapshot
confirm the resulting state. Follow the established `*-state.js`,
`*-island.js`, and `*-actions.js` split when a feature has distinct reactive
state, rendering, and mutation responsibilities; do not create layers that
have no separate ownership job.

### Preact island lifecycle

Every production island uses `mountFeatureIsland` from
`dashboard/js/island-lifecycle.js`. Give it a stable feature name, every host
subtree the feature owns, and a loader callback that dynamically imports the
optional Preact feature graph. The callback returns the feature state (when the
authoritative poll needs it) and a mount function. That function receives
`registerCleanup`; after each render, listener, timer, or subscription is
created, register its idempotent cleanup immediately before starting the next
side effect. The lifecycle can then roll back a partial mount in reverse order.
It claims hosts, registers state, requires cleanup, and keeps load failures
inside the primary host. Do not write another feature-local registry or loader
lifecycle.

Host ownership is exclusive. Non-component code may fetch and publish feature
data but must not independently reconcile or replace a claimed host; the
bounded opaque and transient adapters above are explicit exceptions. Island
cleanup must release effects, listeners, timers, subscriptions, and registry
entries; component tests should call cleanup twice to prove it is idempotent.
Every registered cleanup is attempted even if another throws; host ownership is
released only after they all succeed, so a partial unmount cannot overlap a
later mount. Keep feature modules behind the dynamic loader so a missing
optional asset cannot prevent the static dashboard module graph from linking.

Use `AsyncLoadState` only for the shared accessible loading/error/retry notice.
The feature still owns stale-content layout, request generations, paging,
dialogs, focus policy, and mutations. Extract broader controls only when
multiple features demonstrate the same behavioral contract.

Signals hold durable or derived state. Use effects only to synchronize with an
external system, keep server effects in explicit action modules, use stable
domain IDs as component keys, and reserve imperative refs for widgets or focus
operations that cannot be expressed declaratively.

### Maintained imperative boundaries

Some browser integrations deliberately remain outside Preact reconciliation:

- xterm owns the terminal canvas, hidden textarea, addons, WebSocket stream,
  and terminal-specific input listeners below its stable host;
- `process-graph.js` owns its SVG viewport and pointer mechanics while the
  Processes island owns the surrounding lists, dialogs, and selected data;
- `costs-chart.js` owns its imperative DOM bar chart below the Costs component's host;
- `vegas.js`, `slop-audio.js`, `slop-fx.js`, and `wizard-fx.js` own media,
  audio contexts, transient animation nodes, and their one-way event buses.

Treat these as opaque children: components may mount a stable host and publish
inputs, but must not reconcile the integration's descendants. Lifetimes are
explicit: xterm pane close and graph/chart disposal clean up feature instances,
while the Vegas/slop/wizard binders are installed once for the document lifetime
and do not participate in island cleanup. Moving one behind a component requires
an explicit lifecycle adapter and behavioural coverage for cleanup, focus/input,
and reconnect or animation state.

### Imperative boundary markers

Preact is the default owner for operator-facing markup, drafts, validation,
busy/error state, dialogs, lists, and forms. Static dashboard HTML may provide
an empty stable host, but it must not contain a second dialog implementation.
Snapshot polling and reconciliation must not inspect UI draft state or pause
because an editor is open. Cross-feature `data-act` routing snapshots an
immutable plain descriptor before starting an operation; DOM attributes are
not request or application state.

Imperative code remains only at the following explicit boundaries. A module
that directly creates or injects DOM carries a
`dashboard-imperative-boundary` marker naming a category; the architecture test
discovers markers rather than maintaining a filename allowlist. It accepts seven:
`browser-io`, `config-adapter`, `cost-chart`, `media-effects`, `platform-layout`,
`preact-compat` and `process-graph`. The xterm core and drag and drop rows below
are surfaces with the same contract, not markers.

| Marker / surface | Ownership | Lifetime and disposal | Behavioral test expectation |
|---|---|---|---|
| xterm terminal core | Preact owns terminal tabs, shells, and stable hosts; xterm owns only the opaque descendants handed to `terminals-core.js`. | Terminal close/unmount disposes the terminal, addons, socket, resize observer, and listeners. Reconciliation may retain or retire a host but never rebuild xterm children. | Mount/close/pop-out and roster reconciliation tests must prove opaque-node identity and teardown. |
| `process-graph` | Preact owns editor/dialog state; `process-graph.js` and `process-graph-adapter.js` exclusively own their SVG/canvas-like host. The connector-drop `process-node-chooser.js` owns its anchored combobox/listbox subtree. | The adapter removes pointer/key listeners, connection bands, and graph instances on keyed replacement/unmount. Closing or disposing the chooser removes its document listener and subtree; cancellation restores focus, while unmount disposal leaves focus to the next owner. | Graph interaction tests cover selection, drag/connect, rerender identity, and disposal; chooser tests cover selection, cancellation, focus, click-away, and idempotent disposal. |
| `cost-chart` | Preact owns filters, data derivation, and the chart host; `costs-chart.js` owns the chart's drawing nodes only. | Each effect clears/replaces the chart host and returns cleanup before the next draw or unmount. | Costs tests cover filtered redraw, empty/error states, and chart cleanup without asserting incidental node layout. |
| drag and drop | Preact emits live keyed producers and semantic `data-*` descriptors; `dnd.js`, `group-reorder.js`, `dock-dnd.js`, and `dock-save-dnd.js` adapt native `DataTransfer` events. | Every binder is idempotent, returns cleanup, resets gesture state on `dragend`/unmount, and rejects detached producers. | DnD tests cover copy/move/delete intent, cancellation, cleanup, and live-source guards. |
| `media-effects` | The Vegas audio player and slop/wizard cosmetic modules are boot-time, page-lifetime effects that own media elements, particles, and the explicitly opaque reel host. `drop-landing.js` owns the fixed `#drop-landing-layer` and the blip / in-transit overlays it draws over a drop's landing place. They do not own operational state. | Their delegated document listeners intentionally live until navigation and are not `pageCleanups`. Within that page lifetime, transient nodes self-remove, Vegas stops audio and polling timers when inactive, and identity tokens prevent stale timers from overwriting a newer Preact host. | Reduced-motion, stale-timer, inactive-audio, and opaque reel hand-back tests are required. |
| `platform-layout` | Scoped browser effects own focus traps, resize, horizontal scroll, navigation history, overlay stacking, and stable shell/dock re-homing. The dashboard profile controls are Preact-owned inside named stable hosts; the hosts move between the toolbar and dock while each chip can turn into an inline picker. `island-lifecycle.js` owns only its claimed host's load-failure alert. `peer-view-limits.js` owns one generated `<style>` in the head that greys the controls a peer view refuses. | Effects attach to refs/stable shell nodes and return cleanup. Focus returns to the remounted chip after keyboard cancellation. Island failure rendering replaces only the claimed host; successful cleanup releases host ownership. The peer-view style is recomputed with the snapshot and removed by the limits' disposer. | Binder lifecycle, inline-picker focus, overlay stack, dock identity, refresh-generation, and island rollback/ownership tests cover these contracts; peer-view limit tests cover the generated rule and its removal. |
| `browser-io` | Operation modules may create a short-lived download anchor, clipboard textarea fallback, or standalone Preact host solely to invoke a browser API. `xterm-loader.js` owns the non-visual script node used to fetch the classic xterm runtime on first valid terminal intent. | Temporary operation nodes and object URLs are removed/revoked in the same operation; no draft or request state is stored on them. The xterm script and installed global intentionally live for the page lifetime, while an in-flight promise deduplicates concurrent opens and a failed load is retryable. An auth-aware HEAD preflight preserves the dashboard's expired-session redirect before script injection. | Payload/download/clipboard tests assert the invoked browser contract and cleanup. The xterm loader test proves imports cause no fetch, auth failure causes no injection, concurrent requests append one script, and a ready runtime is reused; facade tests prove canceled/invalid requests do not prepare it. |
| `config-adapter` | `config-form-adapter.js` and `remote-admin.js` are bounded adapters for server-described native controls embedded in Preact-owned config surfaces. | Activation is generation-guarded; option/list replacement is scoped to the supplied control, and the Config island owns activation cleanup. | Config activation and retry tests must prove stale loads cannot publish into a replacement control. |
| `preact-compat` | Preact remains the visual owner. This marker covers trusted legacy readback HTML and standalone process-dialog wrappers used outside the main editor tree, not permission to build new imperative UI. | Injected markup is escaped/trusted at its model boundary; standalone hosts unmount Preact and remove themselves on completion. | Readback escaping and standalone-dialog close/disposal tests are required. New uses need explicit review and documentation here. |

One surface sits just inside that allowance and is worth naming, because it
reads at a glance like an exception: `menu-filter.js`, the type-to-filter core
behind the ⚙ cog menus, decides which items a query keeps by **reading the
rendered menu** rather than from data. It is not new imperative UI — it creates
no nodes, and Preact still owns the box, the query and every item — but it does
mark items on a Preact-rendered subtree. Two properties keep that safe. Its
four `data-menu-*` attributes are declared by no vnode, so Preact never diffs
them and a snapshot publish cannot fight them; and `ActionMenu` re-applies the
filter after every render, so items that appear or disappear with the snapshot
are re-evaluated. Reading the DOM is also the only complete option here: several
items (`NotifyMenuItem`, `RemoteMenuItem`, `RestartMenuItem`,
`SandboxRestartMenuItem`) compute their own label inside the component from live
member state, so no call site knows what they say — and matching the rendered
text means the filter cannot drift from the labels the operator can see.

The guard intentionally allows ordinary ref-based effects (`focus`, measure,
scroll, browser APIs) and rejects undocumented DOM ownership. If a new surface
needs imperative ownership, first prove that Preact cannot own the ordinary UI,
add a documented category/lifecycle/test contract, and then add the source
marker. Do not solve a guard failure with a compatibility re-export, static
dialog markup, or a blanket filename exception.

### Lists that span fleet nodes

A fleet view that pages one list across several nodes (the fused Messages
folders and the fused audit's "all nodes" view are the references) queries each node on demand and never copies a
peer's rows into this node's database, so revoking a peer cuts its rows off
at once. The parts:

- **Server:** the node's list endpoint is bounded and paged in SQL and accepts
  a keyset cursor, `before=<id>`: the page of rows older than that id in the
  node's own newest-first order. It reports `total` (the filtered count),
  `has_more`, and `cursor_paging: true`, so a client can tell it from an older
  build that ignores the parameter. See `mailboxWindow` in
  `pkg/claude/agentd/dashboard_mailbox.go` and `handleDashboardAudit`.
- **Client:** `dashboard/js/fleet-pager.js` turns per-node pages into one
  merged page. A page is a cursor vector (one cursor per node). Page p reads
  one page from each node below its cursor, merges the streams by taking the
  newest head each time, and keeps the first page's worth. The last row taken
  from each node becomes that node's cursor for page p+1. No row is skipped
  or shown twice, a page reads at most nodes × page size rows, and polling
  re-reads the same vector. Counts are the nodes' summed totals.
- **Navigation:** Prev uses the vector kept for the earlier page. There is no
  random access, so a deeper page (Last, or a page restored from the URL) is
  reached by walking forward one read per page (`walkTarget`). Fold each
  merged page into the walk with `recordVector`: it rewrites the next vector
  when the page's boundary moves (new rows on page 1, deletions), and restarts
  the walk when the set of unreachable nodes changes, because a node that was
  down has no cursor in the vectors recorded meanwhile.
- **Mixed versions:** a node without `cursor_paging` is read newest-first to
  the depth the page needs and cut at the cursor client-side
  (`legacyStream`), which stays exact down to the server's largest page.

- **Engine:** `createMergedList` in `fleet-pager.js` does all of the above
  for a list read imperatively (the fused audit view uses it). Give it
  `readPage(node, { before, depth }, ctx)` for one node's page and a refusal
  wording, and it walks, merges, and applies the fleet's liveness rules:
  - A node reported offline is not asked, and it shows as offline, not as
    an error.
  - A node that checks in as online is asked on every refresh. It is only
    given up on when its read fails (a peer read times out after 15s), and
    the message says so.
  - A slow node does not hold up a newly opened page: after
    `SLOW_NODE_MS` the page shows the nodes that answered and names the
    ones still awaited. A refresh keeps the page on screen whole.
  - A walk steps past nodes that just failed, so a silent node costs one
    wait rather than one per page.
- **Pager:** render the footer with `Pager` from `pager.js` (the `pages`
  layout); `stepping` names the page a forward walk is heading for.

Reuse these for any other merged list rather than reading every node down
to the page shown.

## Writing flow tests

Flow tests in `pkg/claude/agentd/*_flow_test.go` are regular Go tests
— they run under bare `go test ./...`. Boundaries (`tmux`, the
`tclaude session new` subprocess) are mocked by assigning fake
implementations to package-wide interface vars (`clcommon.Default`,
`agentd.Spawn`) at test setup, with `t.Cleanup` restoring the
production singletons. No toolchain dependency, no build tag, no
wrapper script.

Flow tests live next to the code they exercise (e.g. `pkg/claude/
agentd/spawn_flow_test.go`) and use the
[`pkg/testharness`](pkg/testharness) DSL. A scenario reads as
Given / When / Then:

```go
package agentd_test

func TestSpawn_RenamesAndResumes(t *testing.T) {
    f := newFlow(t)

    f.HaveGroup("alpha")

    spawn := f.AsHuman().Spawn("alpha", "worker")

    f.AssertSentContains(spawn.TmuxTarget(), "/rename worker", 5*time.Second)
    f.AssertGroupMember("alpha", spawn.ConvID, "worker", "worker", 5*time.Second)

    f.MarkOffline(spawn.TmuxSession)
    resume := f.AsHuman().Resume(spawn.ConvID)
    f.AssertResumeSpawned(resume)
}
```

`newFlow(t)` lives in `pkg/claude/agentd/flow_setup_test.go` and
swaps `clcommon.Default` (Tmux interface) and `agentd.Spawn`
(Spawner interface) for simulator-backed fakes. Scenarios that need
to override further can shadow with another assignment after
`newFlow` returns; the original cleanup still runs and restores the
production singletons.

When adding a new scenario:

1. Pick a verb on `Flow` that captures the action — `Spawn`,
   `Resume`, `Reincarnate`, `Clone`, `Delete` are there. New verbs
   go in `pkg/testharness/flow.go`.
2. Pin the bug class in the test name + a comment paragraph at the
   top — what coordination this guards, what the regression would
   look like.
3. Keep the body short. If a scenario needs more than ~20 lines of
   imperative setup, the harness probably wants a new `Have*`
   helper.
4. Assert at real surfaces (e.g. `GET /v1/groups/{name}/members`,
   `conv.ListSessions`) — not at the simulator's `.jsonl`. The
   simulator writes the file so the real production read path has
   something realistic to walk; the test verifies the surface.
5. When a new Claude Code or tmux quirk bites in production, encode
   it in the simulator (`cc.OnInput` for behavior,
   `cc.SetCommandDelay` for timing) so the regression fails the
   relevant flow test. Over time the sims accrete the institutional
   knowledge of "things that have surprised us".

### Shrinking production timings

Flow fixtures shrink production waits so a scenario costs milliseconds instead
of seconds. Shrinking two related timings *proportionally* is the trap: it
preserves their ratio and discards their absolute separation, and scheduler
jitter is absolute.

TCL-1028 is the worked example. A soft stop runs two concurrent actors with
nothing ordering them: the bounded re-injection ladder spends its attempts by
~8 s, and the escalation watchdog's deadline is 10 s — 2 s of separation that no
amount of runner load can close. The fixture scaled both down, to 1 ms retries
against a 20 ms deadline. Same ratio, and now a single 20 ms deschedule reorders
them. Two scenarios in `soft_exit_retry_flow_test.go` failed intermittently on
loaded macOS runners — in *opposite* directions, over-running and under-running
the same attempt count — while passing 40/40 on an idle Linux box. The fixture
had not modelled production faster; it had moved the system into a regime where
two actors production never races can be reordered by nothing more than a busy
runner.

So before shrinking a wait, ask which other actor it is separated from, and
whether the shrunken separation is still large next to a scheduler pause (tens
of milliseconds on a loaded CI runner). When it is not, do not go looking for a
bigger margin — order the two actors, per "Deterministic timing in tests" above:
park one on a hook, wait for observable state proving the other reached the
point under test, then release. A margin that needs tuning is the symptom; the
missing order is the bug.

## Code conventions

- Focused single-topic commits. See `git log` for the style:
  `agent: …`, `db: …`, `test: …`, `docs: …`.
- HEREDOC commit messages.
- Co-Authored-By trailer on AI-assisted commits — but **no session /
  remote-access links** (`Claude-Session:` trailers,
  `https://claude.ai/code/...` URLs) in commit messages or PR
  descriptions, even if the coding harness's default instructions ask
  for them.
- Make sure your changes at least build (and pass any focused tests
  you relied on) before pushing, but don't duplicate the full
  test/lint suite locally — push early and let CI run:
  ```bash
  go test ./...
  golangci-lint run ./...
  ```
- Don't add comments that restate what the code does. Add WHY
  comments only when the reason is non-obvious (a hidden
  constraint, a workaround for a specific bug, or behaviour that
  would surprise a reader).
- Don't `git add -A` — some dotfiles in the repo root are mounted
  as char devices in dev sandboxes and break the add. Stage by
  path.

## Layout

High-level:

- `pkg/claude/` — main packages (session, conv, agent, agentd, etc.).
- `pkg/claude/common/` — shared utilities (config, db, tmux,
  notify, etc.).
- `pkg/testharness/` — flow-test DSL (CCSim + TmuxSim + Given/When/Then).
- `pkg/common/` — generic utilities (dirs, locking, size parsing).

For feature-level architecture, prefer the focused docs under `docs/` and the
package comments near the code. `CLAUDE.md` / `AGENTS.md` is intentionally only
agent startup context, not a complete architecture inventory.
