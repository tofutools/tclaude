# Remote Control

Drive your agents from your phone or another browser through **Claude Code's
built-in Remote Access** (claude.ai/code + the Claude mobile app). tclaude does
not ship its own remote transport — it arms, tracks, and defaults Claude Code's
native feature so a fleet of agents can be made phone-reachable without touching
each pane by hand.

> **Claude Code only.** Remote control is a Claude Code capability. The Codex
> CLI harness has no built-in remote access, so every remote-control control is
> hidden or rejected for Codex agents (see [Caveats](#caveats)). The per-harness
> matrix lives in [Harnesses](harnesses.md).

## Prerequisite: log in to claude.ai

Remote Access pairs the pane to your Claude account, so the machine running the
agent must be **logged in to claude.ai** (the same OAuth login Claude Code uses).
This is outside tclaude's control — if pairing does not appear in the app, sign
in to Claude Code first. Once paired, an armed agent shows up in the app's
session list.

## Turn it on for one agent

`tclaude agent remote-control` toggles a conversation's Remote Access:

```bash
tclaude agent remote-control            # toggle (default when no intent given)
tclaude agent remote-control on         # arm
tclaude agent remote-control off        # disarm
tclaude agent remote-control status     # read the live pane: on / failed / off
```

`status` reads the agent's live terminal directly — Claude Code draws a `/rc`
pill in its footer while Remote Access is armed — so it answers *"can I connect
right now"* rather than echoing a remembered guess. It reports `on` (armed and
reachable, with the claude.ai/code link), `failed` (armed but the connection
didn't establish), or `off`. Reading the pane also **self-heals** tclaude's
tracked flag, so a manual in-pane toggle no longer leaves the badge stale (see
[Caveats](#caveats)). If the pane can't be read (no live session, or it's too
narrow to draw the pill) `status` falls back to the last-known value and says so.

By default it acts on the calling agent (`self.remote-control`). To drive
another agent, pass `--target` with a title, full conv-id, or 8+-char prefix —
this needs the `agent.remote-control` permission, or being an owner of a group
the target belongs to:

```bash
tclaude agent remote-control on --target worker-3
```

From the **dashboard**, each agent row has a remote-control toggle and a
"📱 remote" badge once armed. See [Agent Dashboard](dashboard.md).

## Turn it on at spawn

Start an agent already phone-reachable with `--remote-control`:

```bash
tclaude session new --remote-control
tclaude agent spawn <group> --remote-control
```

The dashboard's spawn modal has the same opt-in as a checkbox.

## Defaults: profiles and group policy

Rather than arming each agent, set a **default** at two levels. These defaults
pre-fill the spawn form (and apply to spawn paths that send no explicit value);
an explicit per-spawn value always wins. The effective intent is resolved once,
at spawn:

```
explicit per-spawn value  >  group policy (force on/off)  >  profile default  >  off
```

**Spawn profile** — a tri-state "start with remote control" default (unset / on
/ off), set in the dashboard's profile editor. A profile that defaults it on
arms every agent spawned from that profile.

**Group policy** — overrides the profile default, so a whole team defaults on or
off regardless of the profile:

```bash
tclaude agent groups set-remote-control <group> optin    # default ON for the team
tclaude agent groups set-remote-control <group> deny     # default OFF (overrides the profile)
tclaude agent groups set-remote-control <group> inherit  # defer to the profile (the default)
tclaude agent groups set-remote-control <group>          # omit to clear back to inherit
```

A group set to **`optin`** defaults the whole team on; **`deny`** defaults it
off (over an on-by-default profile); **`inherit`** falls through to the profile
default, then off. The dashboard surfaces the policy as a click-to-cycle chip on
the group (inherit → opt-in → deny), and also settable in the dashboard's group
view.

These are **defaults**, not locks: the dashboard spawn modal pre-checks the
"start with remote control" box from this stack (group policy, then the picked
profile's default), but whatever the box shows at submit decides the spawn — so
unticking it for an `optin` team, or ticking it for a `deny` team, is honoured
for that one spawn. The CLI `--remote-control` flag is likewise an explicit
per-spawn value that overrides the group/profile default; a CLI spawn that omits
it falls back to the group policy, then the profile default.

## It survives a relaunch

Resume, reincarnate, and clone recreate the Claude Code pane (a fresh session,
and for reincarnate/clone a new conversation). tclaude re-arms Remote Access on
the new pane from the **source** agent's last-known state, so an agent you armed
for phone access stays reachable across every handoff instead of silently
dropping. (`/clear` keeps the same pane, so it never drops.)

## Caveats

- **The tracked flag can drift — but `status` re-syncs it.** Claude Code has no
  *API-level* readback of whether Remote Access is on, so tclaude keeps its own
  tracked flag and uses it for the dashboard badge and routine display. If you
  type `/remote-control` **directly into the pane** (instead of going through
  `tclaude agent remote-control` or the dashboard), that flag drifts. The fix is
  built in: `tclaude agent remote-control status` (and the direction pick for
  on/off/toggle) reads the live pane's `/rc` footer pill and **self-heals** the
  tracked flag to match reality. So a drifted badge is corrected the next time
  you run `status` or toggle through tclaude. This pane read is **on-demand
  only** — it is never polled, so the dashboard badge still shows the cheap
  tracked value until something explicitly re-checks. One limit: Claude Code
  hides the pill on a too-narrow pane, so on a very narrow terminal `status`
  can't positively confirm `off` and will say the state is unknown.
- **Codex agents have no remote access.** An explicit `--remote-control` or
  `tclaude agent remote-control on` on a Codex agent is rejected; a profile or
  group *default* that would force it on is silently a no-op for Codex (a force-on
  policy never fails a Codex spawn, it just doesn't arm anything).
- **Pairing needs claude.ai.** See the [prerequisite](#prerequisite-log-in-to-claudeai)
  above — without the claude.ai login the agent can be armed but won't appear in
  the app.
