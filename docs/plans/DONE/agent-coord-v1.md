# Agent coordination v1 (PR #47, 2026-05)

The first end-to-end shipping of `tclaude agent` — cross-session
messaging, groups, inbox, and the daemon transport.

## CLI surface

`tclaude agent`:

- `whoami` — print current conversation's ID + display name.
- `lookup` — resolve alias / ID prefix to full conv-id.
- `ls` — list agents reachable to me (members of my groups).
- `message <to>` — send a message to another agent.
- `groups create | rm | ls | members | add | remove`.
- `inbox ls | read`.
- `reply <id>` — reply to an inbox message by ID.

## DB schema (v8)

- `agent_groups` — named groups (allowlist of who-can-talk-to-whom).
- `agent_group_members` — `(group, conv_id, alias?, role?, descr?)`.
- `agent_messages` — `(id, from_conv, to_conv, body, sent_at,
  delivered_at, read_at)`.

## Delivery

- Tmux send-keys nudge when target is online; queued otherwise
  (`delivered_at = ''`).
- Group-shared enforcement — peers must share a group to message.
  Later relaxed: replying to a received message bypasses the
  shared-group requirement.

## Authority

- Mutating-groups gate: refuses if a `claude` / `node` ancestor is
  found in the caller's process tree (so an agent can't silently
  rewrite groups). Absolute in v1 (no `--allow-from-agent` shipped).

## Daemon transport

- `tclaude agentd serve` — HTTP over a Unix domain socket.
- Identity comes from peer credentials (`LOCAL_PEERPID` /
  `SO_PEERCRED`), not tokens. Daemon walks the peer PID to a
  `claude` / `node` ancestor and reads `~/.claude/sessions/<pid>.json`
  for the *current* conv-id — automatically tracks `/fork` /
  `/clear` / `/resume`.
- CLI requires daemon (no DB fallback).

## Skills

Bundled under `pkg/claude/agent/skills/`; installable via
`tclaude setup --install-agent-skills`.

## Refs

- Design: `docs/plans/agent-coord.md`, `docs/plans/agentd.md`.
- PR #47.
