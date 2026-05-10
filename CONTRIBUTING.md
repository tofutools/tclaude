# Contributing

## Quick start

```bash
git clone https://github.com/tofutools/tclaude
cd tclaude
go install .
```

For flow tests (see below), additionally:

```bash
go install github.com/GiGurra/rewire/cmd/rewire@v0.0.75
```

## Running tests

The project has two layers of tests:

| Layer | Files | How to run |
|-------|-------|------------|
| Unit | `*_test.go` (no build tag) | `go test ./...` |
| Flow | `*_flow_test.go`, `//go:build rewire` | `./script/test ./...` or `mise run test` |

`go test ./...` skips flow tests cleanly via the build tag — this is
what bare CI / IDE runs see. Flow tests exercise multi-step
coordination (spawn → /rename → resume, reincarnate-of-r-N, clone
alias derivation, delete cleanup) and need
[rewire](https://github.com/GiGurra/rewire) — a `-toolexec` rewriter
that mocks function boundaries at compile time.

### Option A: mise (recommended)

[mise](https://mise.jdx.dev) auto-installs pinned tool versions and
activates the `[env]` block on `cd`. Run once after cloning:

```bash
mise install   # installs go + rewire
mise run test  # runs the full suite with rewire active
mise run lint  # golangci-lint with rewire build tag
mise run build # plain build (no rewire) using the production cache
```

### Option B: `script/test` wrapper

Works without mise. Useful for CI and devs who don't use mise.

```bash
./script/test ./...
```

The wrapper installs rewire on the fly if missing, sets
`GOFLAGS=-toolexec=rewire -tags=rewire`, and uses a separate
`GOCACHE` (default `${TMPDIR:-/tmp}/go-build-tclaude-test`) so rewritten
compile artifacts don't leak back into the production build cache.

### Caveat: stale rewire registry on first compile

Rewire's first compile of a freshly-mocked package can leave stale
registry entries — symptom is "function X cannot be mocked" on a
target you just added. Two fixes:

```bash
# Either: delete the rewire cache once after adding a new target.
rm -rf "$GOCACHE"

# Or: just run the test twice — second compile picks up the
# scanner's freshly-recorded targets.
./script/test ./pkg/...
./script/test ./pkg/...
```

## Writing flow tests

Flow tests live next to the code they exercise (e.g. `pkg/claude/
agentd/spawn_flow_test.go`) and use the
[`pkg/testharness`](pkg/testharness) DSL. A scenario reads as
Given / When / Then:

```go
//go:build rewire
package agentd_test

func TestSpawn_RenamesAndResumes(t *testing.T) {
    f := newFlow(t)

    f.HaveGroup("alpha")

    spawn := f.AsHuman().Spawn("alpha", "worker")

    f.AssertSentContains(spawn.TmuxTarget(), "/rename worker", 5*time.Second)

    f.MarkOffline(spawn.TmuxSession)
    resume := f.AsHuman().Resume(spawn.ConvID)
    f.AssertResumeSpawned(resume)
}
```

`newFlow(t)` lives in `pkg/claude/agentd/flow_setup_test.go` and
installs the default mocks (FakeTmux + synthesized spawn) via
`rewire.Func`. Scenarios that need extra mocks declare them right
after `newFlow` — later installs win because rewire keys on the
function name.

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

## Build & lint

```bash
go build ./...           # build everything (no rewire)
go test ./...            # unit tests
./script/test ./...      # unit + flow tests
golangci-lint run ./...  # lint (no rewire tag)
golangci-lint run --build-tags=rewire ./...  # lint including flow tests
```

CI runs all of the above on Linux (amd64+arm64) and macOS. Windows
runs the unit tests only — the bash script wrapper doesn't run there
yet.

## Code conventions

- Focused single-topic commits. See `git log` for the style:
  `agent: …`, `db: …`, `test: …`, `docs: …`.
- HEREDOC commit messages.
- Co-Authored-By trailer on AI-assisted commits.
- Pre-flight before staging:
  ```bash
  go build ./...
  go test ./...
  ./script/test ./...
  golangci-lint run --build-tags=rewire ./...
  ```
- Don't add comments that restate what the code does. Add WHY
  comments only when the reason is non-obvious (a hidden
  constraint, a workaround for a specific bug, or behaviour that
  would surprise a reader).
- Don't `git add -A` — some dotfiles in the repo root are mounted
  as char devices in dev sandboxes and break the add. Stage by
  path.

## Layout

See `CLAUDE.md` for the full architecture map. High-level:

- `pkg/claude/` — main packages (session, conv, agent, agentd, …).
- `pkg/claude/common/` — shared utilities (config, db, tmux,
  notify, …).
- `pkg/testharness/` — flow-test DSL (Phase 1 testing-strategy).
- `pkg/common/` — generic utilities (dirs, locking, size parsing).
- `docs/plans/` — design docs and TODO lists.
