# Human-in-the-loop approval flow — design

Even with graduated permissions, sometimes an agent needs to ask the
human "may I do X right now?" out-of-band.

The X-Tclaude-Ask-Human header is shipped on both self-path
endpoints (see `identity.go`) and cross-agent endpoints (see
[`DONE/cross-agent-manager-pattern.md`](../../DONE/cross-agent-manager-pattern.md)
— `requireCrossAgentPermission` honors it as a last-chance escape
hatch).

This file covers the **richer agent-side ask** beyond the existing
header.

## Design sketch

- Agent calls something like `tclaude agent ask --timeout 20s
  --message "Spawn a reviewer agent in group foo?"` on the daemon.
- Daemon opens an approval popup (browser tab, see web-dashboard.md)
  with three outcomes:
  - **ack** — keeps the popup open, cancels the auto-close timeout,
    no decision yet.
  - **approve** — returns success to the requesting agent.
  - **deny** — returns failure.
  - **timeout** — auto-close after N seconds (default 20s) returns
    failure (or "no decision", caller decides).
- Approval is logged so we can audit "who approved what when".

## Implementation

The daemon already has an HTTP server on a Unix socket; pair it with
the browser dashboard (see `web-dashboard.md`) and an ephemeral
approval channel. For inspiration on the popup/ack/timeout UX, see
`/home/gigur/git/oh-shit-meeting` — that project already implements
browser-popup approval with these semantics.

## Open questions

- One-shot grants vs. "remember this answer for N minutes" — useful
  for chatty agents but increases blast radius of a single approval.
- How are approval requests surfaced when no browser tab is open?
  Fall back to a desktop notification + reopening the dashboard?
- Should approvals carry the *full payload* (e.g. the proposed
  message body, the proposed group/member change) so the human can
  see what they're approving? Almost certainly yes.

## Files
- `pkg/claude/agentd/popup.go` — existing approval-popup wiring
- `pkg/claude/agent/` — would add an `ask.go` CLI verb
