# Contributing

## Quick start

```bash
git clone https://github.com/tofutools/tclaude
cd tclaude
go install .
```

## Running tests

```bash
go build ./...
go test ./...
golangci-lint run ./...
```

## Dashboard frontend

### Vendored dependencies

The dashboard uses browser-native ES modules. Preact islands use HTM for
component templates, so editing or running the dashboard requires no Node
install, compiler, or frontend build step: the normal `go install .` workflow
embeds everything it needs. Runtime modules are pinned and committed under
`pkg/claude/agentd/dashboard/vendor/preact/`; the dashboard never loads them
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

Every island exclusively owns the hosts declared by its loader. Imperative code
may fetch or publish data for a feature, but it must not render into, move, or
read application state from an island-owned host. Code outside those hosts may
remain imperative where it has a clear owner; never let Preact and imperative
code write the same subtree.

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
data but must not render into a claimed host. Island cleanup must release effects,
listeners, timers, subscriptions, and registry entries; component tests should
call cleanup twice to prove it is idempotent. Every registered cleanup is
attempted even if another throws; host ownership is released only after they all
succeed, so a partial unmount cannot overlap a later mount. Keep feature modules
behind the dynamic loader so a missing optional asset cannot prevent the static
dashboard module graph from linking.

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
- `costs-chart.js` owns the canvas chart below the Costs component's host;
- `vegas.js`, `slop-audio.js`, `slop-fx.js`, and `wizard-fx.js` own media,
  audio contexts, transient animation nodes, and their one-way event buses.

Treat these as opaque children: components may mount a stable host and publish
inputs, but must not reconcile the integration's descendants. Lifetimes are
explicit: xterm pane close and graph/chart disposal clean up feature instances,
while the Vegas/slop/wizard binders are installed once for the document lifetime
and do not participate in island cleanup. Moving one behind a component requires
an explicit lifecycle adapter and behavioural coverage for cleanup, focus/input,
and reconnect or animation state.

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

## Code conventions

- Focused single-topic commits. See `git log` for the style:
  `agent: …`, `db: …`, `test: …`, `docs: …`.
- HEREDOC commit messages.
- Co-Authored-By trailer on AI-assisted commits — but **no session /
  remote-access links** (`Claude-Session:` trailers,
  `https://claude.ai/code/...` URLs) in commit messages or PR
  descriptions, even if the coding harness's default instructions ask
  for them.
- Pre-flight before staging:
  ```bash
  go build ./...
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
