# Dashboard spawn "auto focus" checkbox

Shipped: the dashboard's "spawn a new agent" modal can pop a terminal
attached to the freshly-spawned agent.

## What shipped

A new **Auto focus** checkbox in the agent-spawn modal (`#agent-spawn-modal`),
default checked. When checked, once the spawn lands the daemon opens a
terminal window attached to the new agent's tclaude session, so the human
can watch and talk to it immediately — a detached spawn otherwise has no
window of its own.

The attach always goes through the `tclaude` wrapper (`tclaude session
attach <label>`), never raw `tmux attach`, so the reattached session keeps
its tclaude features (status bar, window-title stamping, focus/notify
wiring).

## Wire surface

`POST /v1/groups/{name}/spawn` (and the dashboard twin) gained an optional
`auto_focus` boolean in the request body. Opt-in: omitted/`false` ⇒ no
window — the CLI / agent-API default. Only the dashboard checkbox defaults
it on. No new permission gate — opening a window is strictly less than the
`groups.spawn` the handler already requires.

## Persistence (localStorage, client-side)

- `tclaude.dash.spawn.autofocus` — `'1'`/`'0'` checkbox state. Absent ⇒
  defaults to checked. Persisted on each submit so the human's choice
  sticks across spawns.

## Files

- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` reads `auto_focus`
  from the body; after the membership add it calls
  `openTerminal(openAttachCmd(label))` (best-effort, failures logged).
- `pkg/claude/agentd/dir.go` — new `openAttachCmd(label)` builds the
  `tclaude session attach <label>` payload via `clcommon.DetectAbsoluteCmd`.
  Reuses the existing `openTerminal` seam.
- `pkg/claude/agentd/dashboard.html` — checkbox row in the spawn modal,
  `spawnAutoFocusPref()` helper, restore in `openAgentSpawnModal`, read +
  persist + send in `submitAgentSpawn`.
- `pkg/claude/agentd/spawn_autofocus_flow_test.go` — flow tests:
  `TestSpawn_AutoFocusOpensAttachTerminal` (asserts the `openTerminal` seam
  fires with a `session attach <label>` payload),
  `TestSpawn_NoAutoFocusByDefault` (omitted / explicit-false ⇒ no window).
