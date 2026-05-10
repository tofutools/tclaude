# Dashboard: real-time push instead of 5s polling

The dashboard currently polls `/api/snapshot` every 5 seconds.
Works, but:

- Up to a 5s delay before any change shows up in the UI (longer
  if the user just expanded a tree and the next poll arrives
  mid-interaction).
- Wasted bandwidth + CPU when nothing has changed.
- Awkward for live UX (drag/drop optimistic updates, inbox
  arrival, agent state transitions).

This file tracks moving to push-based updates.

## Transports to evaluate

In rough order of weight:

1. **Server-Sent Events (SSE)** — simplest. One-way server →
   browser; the daemon writes events to a long-lived `text/event-
   stream` response. No new dependency on the Go side
   (`net/http`'s flusher is enough). Fits the dashboard's read-
   mostly model; mutations stay on the existing POST/DELETE
   endpoints. Reconnection is built into the browser EventSource
   API.
2. **WebSockets** — bidirectional. Heavier (frame protocol, ping/
   pong, larger client-side library footprint), but lets the
   dashboard send subscriptions / cursor positions back to the
   daemon if that ever matters. Probably overkill for v1.
3. **HTTP/2 server push** — server-initiated stream multiplexing.
   Browser support is uneven and the Go stdlib story is
   awkward. Not recommended.

**Recommendation:** start with SSE. If we hit a wall (e.g.
client-side feedback channel becomes important for the framework
migration), upgrade to WebSockets. Don't pre-commit.

## Event sources

The whole point of pushing instead of polling is that *something*
on the daemon side actually knows when state changed. Today the
daemon's snapshot is constructed on-demand by reading SQLite +
filesystem; nothing emits "snapshot changed" events.

Three event sources exist or are planned:

- **DB writes.** Wherever the daemon writes to SQLite
  (`agent_messages`, `agent_group_members`, `agent_permissions`,
  `agent_cron_jobs`, etc.) we can fan out an event onto a
  broadcast channel. Cheapest to wire up; covers every dashboard
  mutation since they all go through the daemon.
- **fsnotify on `~/.claude/projects/...`** — see
  [`fsnotify-monitor.md`](fsnotify-monitor.md). That TODO already
  exists for unrelated reasons (rename propagation, fresh-conv
  detection). The dashboard live-refresh use case (#2 in that
  file) IS this feature — they should ship together. fsnotify
  detects title / context-pct / new-conv events; the dashboard
  push system delivers them.
- **Tmux session lifecycle.** Today inferred from session-row
  timestamps + a periodic refresh. Could be promoted to events
  if we want online/offline transitions to surface instantly.

**Architectural shape:** a single internal `events` channel /
broadcaster on the daemon. Every source (DB write, fsnotify,
session callbacks) publishes to it. Subscribers (the SSE/WS
handler) filter by type and forward to connected dashboard
clients. Keeps event production decoupled from delivery.

## Coupling with fsnotify-monitor

These two TODOs SHOULD ship together, or at least be designed
together. fsnotify is the event SOURCE for filesystem-driven
changes (title rename, new conv, context-pct); SSE/WS is the
delivery TRANSPORT to the browser. Doing them in either order
without the other gives a half-feature:

- fsnotify alone: daemon learns about changes faster, but the
  dashboard still polls every 5s, so the user sees no
  improvement.
- SSE/WS alone without fsnotify: pushes only fire on dashboard-
  initiated mutations; CC-driven changes (rename, new turn,
  context fill) still need the next poll.

Suggest doing fsnotify first as a one-line "log to stderr on
event" experiment to validate the hypothesis from
`fsnotify-monitor.md`, then designing the events channel + SSE
handler against the validated event shape.

## Auth / scope

Same auth as the existing dashboard endpoints — HttpOnly +
SameSite=Strict cookie + Origin/Referer pinned to the popup base
URL. SSE long-poll connections re-validate the cookie on each
reconnect.

## Backwards compatibility

Keep the polling path working for at least one release. Push is
an optimisation; if the SSE handler breaks the dashboard should
fall back to polling automatically (browser EventSource onerror
→ start the legacy `setInterval` loop).

## Files (when implementing)

- `pkg/claude/agentd/events.go` — new broadcast channel + types.
- `pkg/claude/agentd/sse.go` — SSE handler.
- `pkg/claude/agentd/dashboard.html` — wire EventSource client
  + reconcile incoming events with the local model. (Almost
  certainly post-framework-migration — see
  [`web-dashboard.md`](web-dashboard.md).)
- `pkg/claude/agentd/dashboard.go` — register the SSE route.
- Wherever DB mutations happen — call into the broadcaster.

## Out of scope

- Cross-process daemon → daemon push (federation across hosts).
  See [`future/cross-machine.md`](../future/cross-machine.md).
- Per-tab subscription filtering (only push events relevant to
  the active tab). Optimisation; do once we've felt the cost of
  fan-out in practice.

## Open questions

- Should the SSE stream send full snapshots or per-event diffs?
  Diffs are smaller but require the client to maintain a model;
  full snapshots are simple but defeat the bandwidth win.
  Probably **per-event diffs with periodic full-snapshot
  refresh** as a safety net (every N minutes) so a missed event
  doesn't cause permanent skew.
- Connection limits — how many dashboard tabs can a single user
  open before we cap? SSE is cheap but unbounded fan-out isn't
  free.
