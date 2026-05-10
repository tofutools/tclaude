# Testharness v2 + flow tests (2026-05)

Behaviour-accurate simulators for CC + tmux, plus end-to-end flow
tests that exercise the daemon's HTTP mux without subprocess
boundaries.

## PR #49 — rewire-based flow harness (commit b3f131d)

`pkg/testharness/` with:

- **`CCSim`** owns a real `.jsonl` under
  `~/.claude/projects/<encoded-cwd>/<convID>.jsonl`. Receives
  keystrokes via `Receive(text)`, buffers until `"Enter"` arrives,
  then dispatches through a handler list. Default handlers cover
  `/rename` (writes a `customTitle` turn), `/exit` (final user
  turn + flips alive=false), `/compact` (summary turn), and a
  fallback that writes a user turn. Tests register custom
  behaviours via `cc.OnInput(prefix, handler)` and async-process
  delays via `cc.SetCommandDelay(prefix, dur)`. Zero DB writes —
  CC's job is the `.jsonl`; the daemon owns SQLite.
- **`TmuxSim`** is a pure tmux substitute. `Command(args...)`
  answers `has-session` against an alive flag, routes `send-keys`
  to the attached `CCSim.Receive`, models `kill-session`. Zero DB
  writes.
- **`Flow`** wraps a `World` with a Given/When/Then DSL —
  `HaveGroup`, `HaveAliveSession`, `Spawn`, `Reincarnate`,
  `Clone`, `Delete`, plus surface assertions like
  `AssertGroupMember`, `AssertSentContains`.

## Boundary mocking

Two interface vars in production source — and ONLY two:

- `clcommon.DefaultTmux` — tmux command builder. `LiveTmux{}`
  runs real `tmux -L tclaude …`; tests assign a
  `*testharness.TmuxSim`.
- `agentd.SpawnSpawner` — `tclaude session new` invocations.
  `LiveSpawner{}` forks the real subprocess; tests assign a
  `simSpawner` that builds a `CCSim` + writes the SessionRow the
  production hook callback would have written.

Tests swap these in `flow_setup_test.go` with `t.Cleanup`
restoration:

```go
prevTmux := clcommon.Default
clcommon.Default = m.Tmux
t.Cleanup(func() { clcommon.Default = prevTmux })
```

No toolchain dependency — plain Go interfaces.

## Pinned scenarios

- `TestSpawn_RenamesAndResumes`
- `TestReincarnate_OfRN_ProducesRNplus1`
- `TestClone_EmptyAlias_DerivesFromOriginalTitle`
- `TestDelete_PurgesAllReferencingRows`
- `TestGroupsCreateTeam_BootstrapsMembers`
- `TestGroupsCreateTeam_PerMemberCwdOverride`
- `TestGroupsCreateTeam_BadSpecAbortsBeforeCreate`

## Real CLI sim tests for spawn / --join-group cwd (commit ec04b74)

Replaces stub-fake with testharness-v2 sim test under
`pkg/claude/agentd/spawn_cli_flow_test.go`. Bridges
`agent.DaemonRequestImpl` into the real daemon mux via httptest;
CCSim + TmuxSim are the only fakes. Hoisted `SpawnResponse` to a
named type so real types flow through. Exported `RunSpawn`,
`RunJoinGroup`, `SpawnParams`.

## Assertion philosophy

Verify at **real surfaces**:

- `GET /v1/groups/{name}/members` — what
  `tclaude agent groups members` would render.
- `conv.ListSessions(projectDir)` — what `tclaude conv ls` walks.
- `agent.FreshConvRowResolved` — what the dashboard refreshes
  through.

The simulator's `.jsonl` is impl detail of the mock layer; the
production read path is the system under test. New scenarios
should reach for these surfaces, not poke `.jsonl` files
directly.

When discovering a new CC or tmux quirk that bites in production,
**encode it in the simulator** — `cc.OnInput` for behaviour,
`cc.SetCommandDelay` for timing — so the regression fails the
relevant flow test. Over time the sims accrete the institutional
knowledge of "things that have surprised us."

## Docs

- `docs/plans/testharness-v2.md` — full design.
- `CLAUDE.md` testing section (commit ac14c8b).
