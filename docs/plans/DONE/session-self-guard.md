# Session self-guard — block CC-from-CC spawns — shipped

A Claude Code instance can no longer launch another Claude Code session
directly. `tclaude session new` (and bare `tclaude`, which is a shortcut
for it) now refuses with a non-zero exit when the calling `tclaude`
process is itself running underneath a `claude`/`node` ancestor.

This stops a runaway chain of CC instances spawning each other. It is a
CLI-level guard — distinct from, and complementary to, the daemon's
`requirePermission` gate (see `authority-safety.md`).

## What shipped

- `pkg/claude/session/nested_spawn_guard.go` — new file:
  - `ClaudeAncestorCheck` — package var `func() bool`, the injectable
    seam. Production = `FindClaudePID() != 0` (the existing process-tree
    walk, the same ancestor detection `agentd`'s identity middleware
    uses via `convIDForPID`). Tests substitute a deterministic stub.
  - `ErrNestedClaudeSpawn` — sentinel, matchable via `errors.Is`.
  - `GuardAgainstNestedSpawn() error` — refuses (with an explanatory
    message pointing at `tclaude agent spawn`) when a claude/node
    ancestor is present.
- `pkg/claude/session/new.go` — `runNew` calls `GuardAgainstNestedSpawn`
  after the `--join-group` and pass-through (`--help`/`--version`)
  branches, before any tmux work. `runNew` is the single shared
  RunFunc for both bare `tclaude` (via `pkg/claude.Cmd()` →
  `session.RunNew`) and `tclaude session new` (via `session.NewCmd`),
  so one guard covers both entry points.

## Daemon-initiated spawns are unaffected

`tclaude agent spawn` / `groups resume` make the `agentd` daemon fork
`tclaude session new -d --global`. agentd is started by the human and
is not a CC instance, so a daemon-forked `session new` has agentd (and
the human's shell) in its ancestry — no `claude`/`node` — and the guard
allows it. Agents that need another session use `tclaude agent spawn`,
gated by the daemon's `groups.spawn` permission.

`tclaude session new --join-group` is intentionally *not* blocked by
this guard: it delegates to the daemon, which gates it on `groups.spawn`
— keeping it consistent with `tclaude agent spawn` (its alias).

Known limitation: if the human starts `tclaude agentd serve` from
inside a Claude Code session, daemon-forked spawns inherit that claude
ancestor and would be refused. agentd is expected to run from a plain
shell / login / tray, so this is accepted rather than papered over.

## Escape hatch — decided against

No `--force` flag or env-var bypass. Default-deny is sufficient: the
only legitimate "nested" spawn is the daemon path, which already works
via the ancestry walk, and a human can spawn from a non-CC terminal.
An env-var bypass would also be trivially settable by a CC instance,
defeating the guard. If a concrete need appears, a
`TCLAUDE_ALLOW_NESTED_SPAWN` env var is the obvious minimal addition.

## Tests

- `pkg/claude/session/nested_spawn_guard_test.go` — unit:
  guard refuses when `ClaudeAncestorCheck` returns true (and the error
  wraps `ErrNestedClaudeSpawn`), allows when false; `runNew` is wired to
  the guard and short-circuits before tmux.
- `pkg/claude/agentd/nested_spawn_guard_flow_test.go` — flow:
  `TestDaemonSpawn_NotBlockedByNestedClaudeGuard`. With
  `session.ClaudeAncestorCheck` forced true (guard provably armed), an
  agent peer with `groups.spawn` granted spawns a teammate through the
  daemon and it succeeds — regression guard against the guard leaking
  into the daemon's in-process spawn path.
