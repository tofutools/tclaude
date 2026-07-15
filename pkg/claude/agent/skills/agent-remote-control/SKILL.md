---
name: agent-remote-control
description: >-
  Toggle your own Claude Code Remote Access on/off via `tclaude agent
  remote-control [on|off|toggle|status]`. tclaude agentd injects Claude Code's
  `/remote-control` toggle into your pane, exposing the running session to
  claude.ai/code + the Claude mobile app (after a claude.ai login), gated on the
  `self.remote-control` permission. Use when the user asks to enable/disable
  remote access, reach this session from their phone or the Claude app, or check
  whether it's currently reachable (`status`). Codex CLI has no built-in remote
  access, so this is Claude-Code-only. Manager pattern: `--target <peer>` toggles
  ANOTHER agent's remote access (requires the `agent.remote-control` slug, OR
  being an owner of a group containing the target).
---

# Remote access: reach your session from the Claude app

`tclaude agent remote-control` asks the local `tclaude agentd` daemon to
toggle Claude Code's built-in **Remote Access** on your behalf. When it's
on, your running session is reachable from **claude.ai/code** and the
**Claude mobile app**, so the human can read and steer you from their
phone or another browser without sitting at the terminal.

The daemon does this by injecting Claude Code's `/remote-control` slash
command into your tmux pane — the slash command isn't part of your tool
surface, and the daemon owns the tmux side (same architecture as
`agent-rename` and the lifecycle verbs).

## Prerequisite: logged into claude.ai

Remote Access pairs through claude.ai, so the session must be logged in
via **claude.ai OAuth, not an API key**. If you're running on an API key,
toggling on won't produce a reachable session — tell the human to log in
to claude.ai first.

## Prerequisite: daemon must be running

If you see `Error: tclaude agentd is not running.`, ask the human to
start it:

```bash
tclaude agentd serve   # in a non-sandboxed terminal
```

## Prerequisite: self.remote-control permission

Self remote-control is opt-in. The fastest path is
`tclaude setup --install-default-agent-permissions`, which grants
`self.remote-control` (alongside the other self-lifecycle default slugs —
`self.rename`, `self.compact`, `self.clone`,
`self.schedule`) as defaults in one shot. Manual alternatives:

**Option 1 — globally for every agent.** Either edit
`~/.tclaude/config.json`:

```json
{
  "agent": {
    "default_permissions": ["self.remote-control"]
  }
}
```

…or run:

```bash
tclaude agent permissions grant default self.remote-control
```

**Option 2 — only for one specific conversation.** This grant lives in
SQLite (`agent_permissions`), not config.json. Run:

```bash
tclaude agent permissions grant <conv-id-or-title> self.remote-control
```

If you see `Error: caller is not granted permission "self.remote-control"`,
the human has not opted in. Quote one of the commands above so they know
exactly what to run.

## Toggling

The intent is a positional argument; **`toggle` is the default**:

```bash
tclaude agent remote-control          # toggle (flip current state)
tclaude agent remote-control on       # enable  (no-op if already on)
tclaude agent remote-control off      # disable (no-op if already off)
tclaude agent remote-control toggle   # flip explicitly
tclaude agent remote-control status   # report the live state, don't change it
```

`on` and `off` only act when the state actually differs, so they're safe
to issue blindly; `toggle` always flips. Disabling drives the confirm
menu Claude Code opens — the daemon handles that for you.

`/remote-control` is a toggle with **no programmatic readback**, so
tclaude tracks its own best-known state. `on`/`off`/`toggle` pick their
direction from the **observed live pane** when it can be read (so a flag
that drifted — because a human toggled remote control inside the pane
directly — can't send the toggle the wrong way), and fall back to the
tracked flag when the pane can't be read.

## status: is it actually reachable right now?

`status` reads Claude Code's `/rc` footer pill straight off the live pane,
so it answers "can I connect **right now**", self-heals tclaude's tracked
flag if it had drifted, and prints the connect URL when armed:

```bash
tclaude agent remote-control status
# Remote control is on for abc12345 — observed live, reachable
# Connect at: https://claude.ai/code/...
```

The states it can report:

- **on** — observed live on the pane footer; reachable.
- **ARMED but FAILED** — the pill is up but the session isn't currently
  reachable (e.g. the pairing didn't complete).
- **off** — observed live; not exposed.
- **best-known** — the pane couldn't be read (no live session, or it's too
  narrow to draw the pill), so it reports the last tracked value with that
  caveat.

`status` is read-only and works even with no live pane (it falls back to
the tracked flag). The mutating intents (`on`/`off`/`toggle`) need a live
tmux pane to inject into — otherwise you get `no_tmux` (503).

## Asking the human on denial

`--ask-human <timeout>` turns a permission denial into a human approval
popup with that timeout (e.g. `--ask-human 30s`, capped at 300s; a timeout
counts as deny). It is **self-target only** — it is not honored on the
cross-agent (`--target`) path.

```bash
tclaude agent remote-control on --ask-human 30s
```

## Codex CLI: not applicable

Remote Access is a **Claude Code feature**. Codex CLI has no built-in
remote-access command, so the toggle is unavailable for a Codex-backed
conversation — the daemon refuses it with `unsupported_harness` (409):
"harness codex has no built-in remote access; the remote-control toggle
is unavailable for this agent". There is nothing to configure; it simply
doesn't apply to Codex agents.

## Why a separate command instead of just calling /remote-control

You're a tool-using agent — slash commands inside the TUI aren't part of
your tool surface. Even if you wrote `/remote-control` in chat, the
harness would treat it as plain text, not a command. The daemon owns the
tmux side (and drives the disable-confirm menu), so it can do the toggle
you can't. Same architecture as `agent-rename` and the lifecycle verbs.

## What can go wrong

- **No live tmux session.** A mutating intent with no alive pane returns
  `no_tmux` (503). This usually means you started outside `tclaude` and
  there's no pane for the daemon to reach. Ask the human to wrap your
  session via tclaude.
- **Mid-conversation typing can be lost.** The toggle is delivered by
  keystroke injection into your pane, so anything you'd typed but not
  submitted can get caught up in it. Don't toggle while you have
  unsubmitted input in your textarea.
- **Tracked state can drift.** Because the toggle has no readback, a
  human flipping remote control inside the pane directly can leave
  tclaude's tracked flag stale. Run `status` to observe the live pane and
  self-heal the flag.
- **Toggled on but not reachable.** If `status` reports **ARMED but
  FAILED**, the pill is up but the pairing isn't live — usually a
  claude.ai login issue. Confirm the session is logged into claude.ai.

## Manager pattern: act on ANOTHER agent

`tclaude agent remote-control` accepts an optional `--target <selector>`
that swaps the action onto a peer instead of yourself — e.g. an operator
or lead exposing a worker's pane to the Claude app to watch it from a
phone:

```bash
tclaude agent remote-control on --target worker-1
tclaude agent remote-control status --target worker-1
```

The selector is the same one the rest of `tclaude agent` accepts: the
peer's stable `agent_id` (full or `agt_…` prefix — preferred, since it
survives the peer's own reincarnations), its title, or a conv-id / 8+-char
conv prefix.

Auth model: the caller passes if EITHER

- they hold the `agent.remote-control` slug (default human-only — granted
  via `tclaude agent permissions grant <caller> agent.remote-control`), OR
- they own at least one group that contains the target (mirrors how
  `tclaude agent message` already special-cases group owners).

The response includes `caller_conv` / `caller_agent_id` so the target's
audit trail records who acted. `--ask-human` is **not** honored on the
cross-agent path — the manager pattern is opt-in via explicit grants, not
a popup escape hatch.
