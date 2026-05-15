# agent-dir — report / open the directory an agent is working in

Shipped 2026-05 (prototype / first cut).

Claude Code records a single launch `cwd` in its conversation jsonl —
where it was *started*. That's often not where the agent is actually
building: launched in `~/git`, it may spend the whole session editing
files under `~/git/some-repo/pkg/...`. A tool can't read either value
back, and can't open a terminal window. `tclaude agent dir` closes
both gaps, routed through `tclaude agentd` (which is outside the agent
sandbox).

## What shipped

### Three tracked directories

- **start dir** — where Claude Code was launched (`sessions.cwd`).
- **current dir** — the directory of the most-recent file the agent
  *edited*. The PostToolUse hook records it.
- **worktree dir** — the git working-tree root containing the current
  dir (`git rev-parse --show-toplevel`); falls back to the start dir
  when the current dir isn't in a git repo. Computed on read, not
  stored. `which="worktree"` selects it.

The "current" signal is file edits only (`Edit` / `Write` /
`MultiEdit` / `NotebookEdit`) — `Read` / `Grep` / `Bash` are ignored,
since an agent reads and searches all over while investigating but
edits files in the repo it's working on. Bash `cd` is deliberately not
parsed in this first cut. When no edit has been seen, current falls
back to start (`source: "fallback"` vs `"hook"`).

### DB — schema v26

`agent_workdir(conv_id PRIMARY KEY, dir, updated_at)`, one row per
conv, upserted in place so it stays bounded by conversation count.
Kept as its own table rather than a `sessions` column because
`SaveSession`'s `INSERT OR REPLACE` would clobber an out-of-band
column on every hook tick. Helpers in
`pkg/claude/common/db/agent_workdir.go`:
`UpsertAgentWorkdir` / `GetAgentWorkdir` / `DeleteAgentWorkdir`.

### Hook

No new hook event — `PostToolUse` is already one of tclaude's
installed hooks (`session.RequiredHooks`), and all events funnel
through the unified `tclaude session hook-callback`. The PostToolUse
branch now also calls `WorkDirFromToolUse` (pure, in
`pkg/claude/session/workdir.go`) and `db.UpsertAgentWorkdir`. Existing
installs need no re-setup: the same hook command runs the new binary.

### Daemon endpoints

- `GET  /v1/whoami/dir` — caller's own dirs (`requireAgent`).
- `POST /v1/whoami/dir` — open a terminal (`{"which":"start|current"}`).
- `GET  /v1/agent/{selector}/dir` — another agent's dirs.
- `POST /v1/agent/{selector}/dir` — open a terminal in another's dir.

Both read and open are ungated — reporting a path is harmless, and
opening a terminal is something the human asked for on their own
machine. Terminal spawning goes through `terminal.OpenWithCommand`
(`cd <dir> && exec ${SHELL}`), behind the `openTerminal` package var
seam so flow tests can record without popping a window.

### CLI

`tclaude agent dir [selector] [--start|--worktree] [--open]`:

```
tclaude agent dir                  # current working dir of self
tclaude agent dir --worktree       # git worktree/repo root of self
tclaude agent dir --start          # launch dir of self
tclaude agent dir worker-1         # current working dir of worker-1
tclaude agent dir --open           # open a terminal there
```

Prints the bare path (one line) so it composes in shell.

### Dashboard

Per-agent **term** button (Groups + Agents tabs, online or offline) →
a modal (Current dir / Worktree dir / Launch dir / Cancel) →
`POST /api/term/{conv}` → daemon opens the terminal. Cookie-auth twin
of the `/v1` open path, same as the existing `/api/jump/`.

### Skill

`agent-dir` — bundled, installed by
`tclaude setup --install-agent-skills`. Registered in
`pkg/claude/agent/skills.go`.

## Files

- `pkg/claude/common/db/agent_workdir.go` — table helpers.
- `pkg/claude/common/db/migrate.go` — `migrateV25toV26`, `currentVersion = 26`.
- `pkg/claude/session/workdir.go` — `WorkDirFromToolUse` (pure derivation).
- `pkg/claude/session/hook_callback.go` — `tool_input` field + PostToolUse tracking.
- `pkg/claude/agentd/dir.go` — `/v1` dir handlers + `/api/term/` + `openTerminal` seam.
- `pkg/claude/agentd/serve.go`, `agent_dispatch.go`, `dashboard_edit.go` — routing.
- `pkg/claude/agentd/dashboard.html` — term button + modal + dispatch.
- `pkg/claude/agent/dir.go` — `tclaude agent dir` CLI.
- `pkg/claude/agent/agent.go`, `skills.go` — command + skill registration.
- `pkg/claude/agent/skills/agent-dir/SKILL.md` — the skill.

## Tests

- `pkg/claude/session/workdir_test.go` — `WorkDirFromToolUse` table test.
- `pkg/claude/agentd/dir_flow_test.go` — flow tests: reports start +
  current, falls back when no edit, open spawns terminal in the right
  dir, dashboard term button.

## Open / deferred

- Bash `cd` is not parsed — an agent that only runs shell commands
  won't move its current dir off the launch dir.
- No permission gate on `--open`; if a terminal-window popup ever
  feels too loose, add a `self.dir-open` / `agent.dir-open` slug.
- `agent_workdir` rows aren't cascade-deleted with the conv;
  `DeleteAgentWorkdir` exists but isn't yet wired into `conv delete`.
- The worktree dir shells out to `git` on every read (no caching);
  fine at human-click rates, worth a cache if it ever gets hot.
