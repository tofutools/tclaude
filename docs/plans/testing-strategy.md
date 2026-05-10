# Testing strategy

## Why this exists

We have decent unit-test coverage on individual handlers and DB
functions, but several recent bugs only surfaced when commands
trigger automation that triggers more commands —
spawn → `/rename` → resume, reincarnate-of-reincarnate index
collision, clone alias derivation, the silent "resumed" lie. These
**flow** bugs are exactly what unit tests miss: each piece works in
isolation, the interaction breaks.

This doc proposes a testing strategy that makes flows the *primary*
unit of regression coverage, while keeping everything in `_test.go`
(no separate binaries, no daemonized test infra).

## The shape of the problem

Flows in this system look like:

```
human/agent runs CLI
  → CLI hits daemon over Unix socket
    → daemon writes to DB
    → daemon spawns a subprocess (`tclaude session new …`)
      → CC starts up inside tmux
        → CC's hook callback hits the daemon (HTTP)
          → daemon writes more to DB
    → daemon sends-keys to CC's tmux pane
      → CC processes the slash command / prompt
        → CC writes to .jsonl
        → CC fires another hook
          → daemon reads the new state, may inject more, may reply…
```

Multiple processes, multiple sync points, both timer-driven (poll
loops) and event-driven (hooks). Unit-testing the daemon alone
misses everything past "daemon writes to DB". Real e2e
(subprocess + tmux + mock CC binary) covers everything but is
heavy, flaky, and platform-sensitive.

## The chosen approach: rewire + synthetic CC, all in-process

### Components

1. **Rewire** ([gigurra/rewire](https://github.com/GiGurra/rewire))
   for mocking the messy boundaries — `clcommon.TmuxCommand`,
   `exec.Command`, `os.Getwd`, `time.Now`, `session.IsTmuxSessionAlive`,
   `db.LoadSession` polling, etc. Production code stays unchanged;
   the mocks are scoped to a `*testing.T` and auto-restore.

2. **`pkg/testharness`** — a new top-level test-support package that
   provides:
   - `World` — owns `t.TempDir()` for `$HOME` / `~/.claude`,
     a fresh SQLite test DB, the daemon's HTTP handlers (callable
     directly via `httptest.NewRecorder`), and a `CCSimulator`.
   - `CCSimulator` — a plain Go struct (NOT a mock). Drives the
     synthetic CC behaviour: writes `.jsonl` content on cue, fires
     hook callbacks, materializes conv-ids, records `send-keys`
     input that the daemon would inject. Tests script it.
   - `FakeTmux` — backing store for `clcommon.TmuxCommand` rewires:
     records `send-keys`, fakes `has-session`/`list-sessions`,
     marks sessions alive on `new-session`. Lives behind one
     rewire on `clcommon.TmuxCommand` so existing call sites stay
     untouched.
   - `FakeClock` — `time.Now` rewire, `Advance(d)` for tests that
     need to traverse polling loops without `time.Sleep`.

3. **`httptest.NewRecorder` + direct handler calls** — the daemon's
   HTTP handlers are just `func(w, r)`. We already use this in
   `pkg/claude/agentd/handlers_messages_test.go`. Same pattern
   scales to flow tests; no socket needed.

4. **Scenario tests** — flow tests live next to the code they
   exercise (`pkg/claude/agentd/spawn_flow_test.go`,
   `…/reincarnate_flow_test.go`, etc.). Each scenario reads as a
   sequential script:

   ```go
   func TestSpawn_RenamesAndPersistsJsonl(t *testing.T) {
       w := testharness.New(t)
       defer w.Close()

       resp := w.PostJSON(t, "/v1/groups/alpha/spawn",
           map[string]any{"alias": "worker"})
       require.Equal(t, 200, resp.Code)

       w.CC.MaterializeConvID(resp.Label, "abc-123")
       w.AdvanceTilTmuxAlive("abc-123")

       require.True(t, w.Tmux.WasInjected("abc-123:0.0",
           "/rename worker"))
       require.True(t, w.JsonlExists("abc-123"))

       // resume should now find the conv
       resume := w.PostJSON(t, "/v1/agent/abc-123/resume", nil)
       require.Equal(t, 200, resume.Code)
   }
   ```

### What rewire mocks (concretely)

- `clcommon.TmuxCommand` → `FakeTmux.Command(...)`. Captures
  args, drives a fake session table the assertions read from.
- `exec.Command` for `tclaude session new` shell-outs (used in
  `spawnDetachedTclaudeNew/Resume`) → records the spawn intent.
  The `CCSimulator` then synthesizes the resulting CC behaviour
  on demand.
- `time.Now` (only in tests that hit the polling loops) → driven
  by `FakeClock`.
- `session.IsTmuxSessionAlive` → reads `FakeTmux`'s session table.
- `os.UserHomeDir` → returns the test's tmpdir, so the daemon's
  `~/.claude/projects/...` writes land in scratch space without
  any global env mutation.

### What `CCSimulator` provides (plain Go, no mocking)

```go
type CCSimulator struct {
    World *World
    // ...
}

// MaterializeConvID simulates CC writing its conv-id into the
// session row that tclaude polls during spawn. Mirrors the hook
// callback the real CC fires shortly after launch.
func (s *CCSimulator) MaterializeConvID(label, convID string)

// AdvanceTilTmuxAlive flips the FakeTmux session for convID to
// "alive", unblocking waitForConvAlive's poll loop.
func (s *CCSimulator) AdvanceTilTmuxAlive(convID string)

// WriteJsonl appends a turn to the synthetic .jsonl. Used to
// simulate CC processing an injected /rename or message.
func (s *CCSimulator) WriteJsonl(convID string, turn JsonlTurn)

// Hook fires a hook callback against the daemon's HTTP handler,
// as the real CC would after PostToolUse / Notification / etc.
func (s *CCSimulator) Hook(name, sessionID string, payload any)

// ProcessInjection reads the next pending FakeTmux send-keys and
// applies it: /rename writes a custom_title turn, "[system: …]"
// system messages append a user turn, etc. Tests call this to
// "advance" CC's processing of injected input.
func (s *CCSimulator) ProcessInjection(convID string)
```

`CCSimulator` is the centerpiece — flow tests are a sequence of
`PostJSON(...)` + `CC.X(...)` calls, with `require.X` between.

### File layout

```
pkg/testharness/
├── world.go        // World struct, constructor, Close
├── cc_sim.go       // CCSimulator
├── tmux_sim.go     // FakeTmux + clcommon.TmuxCommand rewires
├── clock.go        // FakeClock + time.Now rewire
├── http.go         // World.PostJSON / DeleteJSON / GetJSON helpers
├── jsonl.go        // synthetic .jsonl read/write helpers
└── doc.go          // package doc with usage example

pkg/claude/agentd/<verb>_flow_test.go   // one per flow
pkg/claude/agent/<verb>_flow_test.go    // CLI-level flows if needed
```

Existing unit tests (`handlers_test.go`, `clone_test.go`, etc.)
stay as-is — they cover the smaller pieces that don't need a flow
context.

## Toolchain plumbing — making it Just Work for other devs

Rewire requires:

- The `rewire` binary in PATH.
- `GOFLAGS=-toolexec=rewire` when running tests.
- A separate `GOCACHE` for tests so rewire-rewritten cache entries
  don't pollute the production build cache (per rewire's setup
  docs).

We'll combine **mise** (auto-activated for mise users) with a
**`script/test`** wrapper (works for everyone, including CI).

### `mise.toml` at repo root

```toml
[tools]
go = "1.23"          # match go.mod
"go:github.com/GiGurra/rewire/cmd/rewire" = "latest"

[env]
GOFLAGS = "-toolexec=rewire"
GOCACHE = "{{ env.HOME }}/.cache/go-build-tclaude-test"

[tasks.test]
description = "Run go test with rewire"
run = "go test ./..."

[tasks.lint]
run = "golangci-lint run ./..."

[tasks.build]
description = "Plain build (no rewire). Uses default GOCACHE."
env = { GOFLAGS = "", GOCACHE = "{{ env.HOME }}/.cache/go-build" }
run = "go build ./..."
```

The `"go:..."` tool spec uses mise's go-install backend — when a
dev runs `mise install` (or `mise i`) at the repo root, mise
shells out to `go install github.com/GiGurra/rewire/cmd/rewire@latest`
and puts the binary on PATH. No separate `go install` step
needed. `cd`-ing into the repo then activates the env vars from
`[env]` automatically. `mise run test` runs the suite.
Production builds via `mise run build` use the clean cache and
clear GOFLAGS, so they're never rewire-rewritten.

Note: mise warns about missing tools on `cd` but doesn't
auto-install — devs run `mise install` once after cloning. The
`script/test` path below auto-installs as a fallback for devs
who skip mise.

### `script/test` — universal entry point

```bash
#!/usr/bin/env bash
# Run the test suite with rewire mocks.
# Works without mise. Useful for CI and devs who don't use mise.

set -euo pipefail
cd "$(dirname "$0")/.."

# Install rewire if missing.
if ! command -v rewire >/dev/null 2>&1; then
    echo "Installing rewire…" >&2
    go install github.com/GiGurra/rewire/cmd/rewire@latest
fi

export GOFLAGS="-toolexec=rewire"
export GOCACHE="${GOCACHE:-$HOME/.cache/go-build-tclaude-test}"

exec go test "$@" ./...
```

CI calls `script/test` directly. Devs who don't want mise just run
`./script/test`. Devs who use mise get `mise run test`.

### `//go:build rewire` build tag for the flow tests

A dev who runs `go test ./...` without rewire (no GOFLAGS, no
script wrapper) would hit `rewire.Func` panics in flow tests. To
avoid breaking that path:

```go
//go:build rewire

package agentd_test
```

`script/test` adds `-tags=rewire`. Bare `go test ./...` skips the
flow tests entirely (still runs everything else). Devs see "ok,
0.x% of tests skipped" rather than panics.

CI runs both: `go test ./...` for the unit layer, `script/test`
(or `mise run test`) for the flow layer.

### CI

GitHub Actions:

```yaml
- uses: jdx/mise-action@v2
- run: mise run test
- run: mise run lint
```

Or without mise:

```yaml
- run: go install github.com/GiGurra/rewire/cmd/rewire@latest
- run: ./script/test
```

Either works. Both are documented.

## Initial scenarios to write

Pick a small set that exercises the flows that have bitten us:

1. **Spawn → rename → resume.** Spawn produces a `.jsonl`,
   resume successfully reattaches.
2. **Reincarnate of `r-N`.** New title is `r-(N+1)`, old conv
   is archived, follow-up is delivered.
3. **Clone with empty alias.** New clone gets `<base>-c-1` based
   on the original's display title, not bare `c-1`.
4. **Multi-recipient send.** N+1 rows land, every receiver's
   `inbox read` shows the audience.
5. **Delete cleanup.** All referencing rows gone, `.jsonl` gone,
   session-env gone, idempotent re-delete is a no-op.
6. **Resume of an orphan.** Pre-bc7ec81 orphan (no `.jsonl`)
   surfaces a clear error rather than the silent "resumed" lie
   we shipped.

Aim for one scenario test per file, ~50 lines each. The point
is that *anyone reading the test* understands what flow it
guards.

## Open questions

- **Bus factor on rewire.** It's @gigurra's project. If a future
  Go release breaks the toolexec rewriter and there's no quick
  fix, the test infra blocks. Mitigations:
  - Pin a known-good rewire version (mise tool spec pins the
    version; CI verifies).
  - Vendor rewire? Probably overkill — we'd be on the hook for
    keeping it in sync.
  - **Escape hatch** documented up front: if rewire breaks
    irrecoverably, the migration path is to introduce
    `TmuxClient` / `ProcSpawner` / `Clock` interfaces (~15-20
    callsites) and switch the harness to inject them. Painful
    but bounded.

- **Real-binary smoke test.** Synthetic e2e misses bugs in real
  tmux paste-mode, real subprocess signal handling, real CC
  wire-protocol quirks. Worth keeping ONE real-binary scenario
  that runs nightly or on a manual trigger. Not on every PR.
  Scope to be decided once the synthetic harness lands.

- **Flow test runtime budget.** Each scenario should be
  sub-second. If they grow slow we'll need a `-short` flag
  or split them into a separate `make test-flow` target.

- **CC behaviour fidelity.** `CCSimulator` simulates only what
  the daemon depends on — conv-id materialization, hook
  callbacks, .jsonl writes triggered by injection. Real CC has
  many more behaviours (compaction, sub-agents, the agent
  loop). We add to the simulator only when a flow needs it,
  not preemptively.

## Rollout

Phased so we don't block on a big-bang refactor:

1. **Phase 1 (small):** add the `pkg/testharness` skeleton with
   World + FakeTmux + one rewire on `clcommon.TmuxCommand`.
   Write the spawn → rename → resume scenario. Add `script/test`.
   Validate the approach end-to-end on this single test.

2. **Phase 2 (broaden):** mise.toml lands. Rewire remaining
   boundaries (`exec.Command`, `time.Now`, `os.UserHomeDir`).
   Add scenarios 2-6.

3. **Phase 3 (CI):** wire `script/test` into the GitHub Actions
   workflow. Document setup in CONTRIBUTING.md (or
   `docs/contributing.md` — there isn't one yet).

4. **Phase 4 (smoke):** add the real-binary nightly test.
   Strict scope: one happy-path spawn-and-message scenario,
   skipped on PR runs, fired on `schedule:` cron.

If Phase 1 reveals rewire is too painful for our setup, we have
the interface-refactor escape hatch and we lose only the time
spent on the harness skeleton (~half a day). Cheap to find out.
