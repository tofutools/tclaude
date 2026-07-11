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

The dashboard uses browser-native ES modules. Preact islands use HTM for
component templates, so editing or running the dashboard requires no Node
install, compiler, or frontend build step: the normal `go install .` workflow
embeds everything it needs. Runtime modules are pinned and committed under
`pkg/claude/agentd/dashboard/vendor/preact/`; the dashboard never loads them
from a CDN.

Dependency upgrades are deliberately rare and reviewed as vendored-code
changes. Update the exact versions, hashes, source maps, and license metadata
in that directory's `README.md` together, then run the ordinary Go test suite.

### Writing Preact component tests

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

Flow tests in `pkg/claude/agentd/*_flow_test.go` are regular Go tests
— they run under bare `go test ./...`. Boundaries (`tmux`, the
`tclaude session new` subprocess) are mocked by assigning fake
implementations to package-wide interface vars (`clcommon.Default`,
`agentd.Spawn`) at test setup, with `t.Cleanup` restoring the
production singletons. No toolchain dependency, no build tag, no
wrapper script.

## Writing flow tests

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
