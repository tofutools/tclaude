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
tclaude agent remote-control status     # print tclaude's best-known state
```

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

Rather than arming each agent, set a **default** at two levels. The effective
intent is resolved once, at spawn:

```
group policy (force on/off)  >  explicit per-spawn opt-in  >  profile default  >  off
```

**Spawn profile** — a tri-state "start with remote control" default (unset / on
/ off), set in the dashboard's profile editor. A profile that defaults it on
arms every agent spawned from that profile.

**Group policy** — overrides the profile, so a whole team can be forced on or
kept off regardless of the profile:

```bash
tclaude agent groups set-remote-control <group> optin    # force ON for the team
tclaude agent groups set-remote-control <group> deny     # force OFF (overrides the profile)
tclaude agent groups set-remote-control <group> inherit  # defer to the profile (the default)
tclaude agent groups set-remote-control <group>          # omit to clear back to inherit
```

A group set to **`deny`** is an *absolute* off — it keeps a sensitive team
unreachable even if an agent is spawned with an explicit `--remote-control` or
from an on-by-default profile. A group set to **`optin`** arms the whole team. A
group left at **`inherit`** falls through to the explicit per-spawn opt-in, then
the profile default, then off. The dashboard surfaces the policy as a
click-to-cycle chip on the group (inherit → opt-in → deny). Group policy is also
settable in the dashboard's group view.

## It survives a relaunch

Resume, reincarnate, and clone recreate the Claude Code pane (a fresh session,
and for reincarnate/clone a new conversation). tclaude re-arms Remote Access on
the new pane from the **source** agent's last-known state, so an agent you armed
for phone access stays reachable across every handoff instead of silently
dropping. (`/clear` keeps the same pane, so it never drops.)

## Caveats

- **Best-known state can drift.** Claude Code exposes no programmatic readback
  of whether Remote Access is on, so tclaude tracks its own *best-known* state
  and uses it to decide which way the toggle should go. If you type
  `/remote-control` **directly into the pane** (instead of going through
  `tclaude agent remote-control` or the dashboard), tclaude's tracked state can
  fall out of step with reality — the badge or the next toggle may then be
  wrong. Re-sync by toggling once through tclaude (or check
  `tclaude agent remote-control status`). Prefer the tclaude/dashboard controls
  over typing the slash command yourself.
- **Codex agents have no remote access.** An explicit `--remote-control` or
  `tclaude agent remote-control on` on a Codex agent is rejected; a profile or
  group *default* that would force it on is silently a no-op for Codex (a force-on
  policy never fails a Codex spawn, it just doesn't arm anything).
- **Pairing needs claude.ai.** See the [prerequisite](#prerequisite-log-in-to-claudeai)
  above — without the claude.ai login the agent can be armed but won't appear in
  the app.
