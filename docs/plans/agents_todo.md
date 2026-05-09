# Agent coordination — TODO / DONE

Living todo list for agent coordination work in tclaude. Update as items
ship or get scoped out. The detailed v1 design lives in
[`agent-coord.md`](agent-coord.md).

---

## In progress

(nothing currently being worked on — pick from TODO below)

---

## TODO

### Session shortcuts
- `tclaude --join-group <group>` — start a fresh session and auto-join the
  given group on first message.
- `tclaude --join-group <group> --agent-name <name> [--agent-role <role>]
  [--agent-descr <text>]` — combine session creation, conv rename, group
  join, and role assignment in one command.
- Decide where the join happens: pre-launch (DB only) vs first-tick
  (after the conv-id is known). Probably first-tick via a hook.

### Group lifecycle (spawn / stop / resume entire teams)

The big idea: a **group is a persistent team** the human (or a
trusted agent) can spawn on demand, suspend, and resume. This is the
load-bearing UX for "delegate this batch of work to a code-reviewer +
test-runner + integration-runner team, then come back later."

The membership table already exists; what's missing is operations
that *act on* members in bulk.

- `tclaude agent groups spawn <group>` — for each member of the group,
  start (or re-attach) a `tclaude` session running CC, register it
  against that member's `conv_id`, and place its tmux pane in a known
  state. Two cases per member:
  - **Has a live conv** with a dead tmux session → resume into a fresh
    tmux session with that conv-id (we already have
    `tclaude session resume`).
  - **No conv yet** (member added but never spawned) → start a fresh
    CC session, capture the conv-id on first hook, and overwrite the
    placeholder member row's conv_id. Open question: do we let the
    human pre-fill `member.role` / `member.descr` and pass them as a
    bootstrap prompt the spawning agent receives on first turn?
  - Idempotent: spawning a group whose members are all already online
    is a no-op (useful as a "make sure my team is up" reconciliation).

- ~~`tclaude agent groups stop <group>`~~ — **shipped**. Soft default
  (inject `/exit` via tmux send-keys), `--force` does
  `tmux kill-session`. Per-member result table. Membership preserved.
  Permission slug `groups.stop`.

- ~~`tclaude agent groups resume <group>`~~ — **shipped** for the
  has-conv-but-dead-tmux case. Spawns
  `tclaude session new -r <conv> -d --global` for each offline
  member; idempotent. Permission slug `groups.resume`. The
  no-conv-yet placeholder case is Phase B (templates).

- `tclaude agent groups create <group> --team <template>` — bootstrap
  a group + initial members in one call. Template is JSON or a flag
  bundle:
  ```
  tclaude agent groups create reviewer-team \
    --member alias=lead,role=tech-lead,descr="...",cwd=. \
    --member alias=tester,role=test-runner,descr="..."
  ```
  Each member starts in the `no-conv-yet` placeholder state until
  `groups spawn` is called.

- `tclaude agent groups archive <group>` — soft-delete (so message
  history stays queryable but membership is sealed). Distinct from
  `stop`: archive freezes the membership too. Probably implies
  `stop --force` first.

- **Per-row online filters** (already in the Discovery section but
  worth restating here) so `groups ls --state=offline` surfaces
  groups whose teams aren't currently running — natural input to
  "which teams need spawning?".

**Permission slugs to add** (so all of this is delegatable to agents,
not just human-only). All gated by default — consistent with the
existing `groups.*`/`member.*` model:

- `agent.spawn` — start a new tclaude/CC session for a conv (or for
  a placeholder member). The single most powerful slug we'd add: an
  agent that holds it can effectively run code on the human's
  machine via CC. Default: nobody.
- `agent.stop` — terminate another conv's session (tmux exit / kill).
- `agent.resume` — re-attach a previously-stopped session.
- `groups.spawn` — bulk version of `agent.spawn` over a group's
  members. Holding `groups.spawn` implies holding `agent.spawn` for
  every conv in groups the agent can see (or we keep them
  independent — design choice).
- `groups.stop` / `groups.resume` — bulk versions, scoped to a
  group.
- `groups.archive` — soft-delete a group. Lower-blast-radius than
  `groups.rm` since the messages stay.

**Recommended UX progression for the human**:
1. Manage teams from the CLI: `groups create --team`, `groups
   spawn`, `groups stop`. Reads like docker-compose for agents.
2. Eventually do the same from the dashboard (one-click spawn /
   stop a team, pending-approval queue inline).
3. Grant a *coordinator agent* `groups.spawn`/`groups.stop` so it
   can manage subordinate teams without bothering the human (with
   `--ask-human` as the off-ramp for one-off escalations).

**Open questions:**
- How do member rows survive across spawn cycles? If we want
  conv-id stability (so `permissions grant <conv> ...` keeps
  working across spawns), we have to track a "logical member id"
  separately from the conv-id, or accept that re-spawning a
  no-conv-yet member produces a brand-new conv. Probably the
  latter: members are templates; conv-ids are runtime state.
- Should `stop` be reversible (`resume` brings the same conv-ids
  back) or "kill and recreate"? Reversible is much nicer for the
  human ("I want to suspend this team for an hour"); recreate is
  simpler.
- Where do we store team templates? If `--member alias=...,role=...`
  flags get cumbersome, a `~/.tclaude/teams/<name>.toml` directory
  would feel natural — same shape as docker-compose / k8s manifests.
- Bootstrap prompts (the message a freshly-spawned member sees as
  its first `[system: ...]` nudge) need a home. Probably a
  per-member optional `bootstrap_prompt` column that gets injected
  on first `agent.spawn`.

### Discovery / state
- `tclaude agent groups ls --state=online|offline` — filter by whether
  members have a live tmux session right now. (Online count column
  already shipped; this is just a filter on top.)
- `tclaude agent ls --state=online|offline` — same filter, but for peers.
- Per-row online indicator on `agent ls` and `groups members` (e.g.
  `●`/`○` or `yes`/`""` column). The `isConvOnline` helper used by
  `groups ls` makes this trivial to extend.
- Selectable filtering: pressing `g` in `conv ls -w` could open a fuzzy
  group picker. (Groups column itself is shipped.)

### Inbox & message UX
- **Interactive mailbox inspector**: `tclaude agent mailbox <conv> -w` (or
  some better verb — possibly `inbox watch`, `mail`, etc.). Lists mails
  with sender/subject/date, lets the user select one to read, marks read
  on view, supports reply. Reuse `pkg/claude/common/table` (the same
  interactive table that backs `conv ls -w` and `session ls -w`) so
  filtering, sorting, and key bindings feel consistent. Two views are
  probably useful:
  - `tclaude agent mailbox <agent>` — that agent's inbox (the operator's
    debugging/auditing view).
  - `tclaude agent mailbox` (no arg) — current conversation's inbox,
    intended to be invoked by a running agent that just got nudged.
- ~~Surface outbox via `inbox sent`.~~ **Shipped.** `tclaude agent
  inbox sent` lists this conv's outgoing messages with delivery +
  read status from the recipient's side. JSON via `--json`.
- Multi-recipient messages: add `to_convs` (or normalise to a
  per-recipient row table) plus a `cc_convs` list. The "from / to / cc /
  subject / body / read" mental model maps directly onto email and is
  intuitive for agents to reason about.
- Optional Reply-To / In-Reply-To threading so an agent can quote what it
  is replying to. Lightweight: just a nullable `parent_id` column on
  `agent_messages`.
- On session resume, flush undelivered nudges (`delivered_at IS NULL`) so
  messages sent while the target was offline still get surfaced.
- `tclaude agent inbox prune --older-than 30d --read-only` — delete
  `agent_messages` rows whose `read_at` is set and older than the
  cutoff. **TODO:** until this exists, message rows accumulate forever
  in SQLite. Bodies are short, so this is fine for a long while, but
  the option should exist.
- Conversation thread IDs surfaced to agents (so a reply can quote
  the parent). v1 just records `from`/`to`/`group`.

### Authority / safety
- v1 detector for mutating `groups create|rm|add|remove`: walk the parent
  process tree; if any ancestor is `claude` (or `node`, since CC runs as
  node), refuse by default. Override via `agent.allow_agent_mutate_groups`
  in `~/.tclaude/config.json` or per-call `--allow-from-agent`.
- Possible refinement: more granular config, e.g. allow `add` but not
  `rm`/`create`. Useful if we want agents to self-onboard into known
  groups.
- Possible refinement: extend the same gate to other sensitive commands
  (e.g. spawning new sessions, killing groups via `groups stop`). Map
  command → required policy in config.

### Membership maintenance
- **Redesignate members in place.** Once the human has added an agent to
  a group, the alias / role / descr should be editable without removing
  and re-adding. Probably:
  ```
  tclaude agent groups update-member <group> <conv> [--alias …] [--role …] [--descr …]
  ```
  Same human-only gate as `add`/`remove`. Useful when an agent's purpose
  in a group shifts mid-flight, or when `--agent-name` was wrong.

### Default agent permissions in tclaude config (v1 shipped)

V1 is in: `~/.tclaude/config.json` accepts an `agent` section with
`default_permissions` and `permission_overrides[conv|prefix|title]`.
The daemon's `requirePermission()` consults overrides → defaults →
refuses. Humans (no CC ancestor) bypass the check entirely.

Open follow-ups:
- More granular gates on the existing `groups …` mutating endpoints
  (currently absolute via `requireHuman`; want them to also accept a
  permission like `member.redesignate`).
- Wildcard / pattern overrides (e.g. `"role:reviewer": [...]` instead
  of pinning to a single conv-id).

### Agent self-service permissions (graduated trust)

Today the human is the sole mutator: any `groups create|rm|add|remove`
from a process with a `claude`/`node` ancestor is refused. We want a
graduated permission model so trusted agents can do *some* of this on
their own, while everything else still requires human action.

Permission levels to consider, from least to most powerful:
- `member.redesignate` — change alias / role / descr on existing members
  (incl. self).
- `member.add` — add a conv to one of *its own* groups (self-onboard
  peers it can already see).
- `member.remove` — kick a conv from one of its own groups.
- `agent.spawn` — start a new tclaude session (probably implies a
  bootstrap group join, see "Session shortcuts").
- `agent.stop` — terminate another agent's session (tmux kill).
- `agent.resume` — re-attach a stopped session.
- `groups.create` / `groups.rm` — full group lifecycle.

Storage shape: a per-`(conv, group)` permission set, plus a per-conv
"global" permission set the human can grant for cross-group powers
(`agent.spawn` doesn't really belong to any one group). The daemon's
`requireHuman()` becomes a permission check against this table; the
"absolutely no caller from a `claude` ancestor" rule remains the
default for any permission the agent does not hold.

Open questions:
- Are permissions inherited (if `member.add` is granted in group X, can
  the agent add to *any* group it's a member of?). Probably no — keep
  it explicit per group, with a separate "global" bucket.
- How does a permission grant happen? Likely `tclaude agent groups
  grant <group> <conv> member.add` (human-only, same gate as
  membership).
- Should grants expire? v1: no; persistent until revoked.

### Human-in-the-loop approval flow

Even with graduated permissions, sometimes an agent needs to ask the
human "may I do X right now?" out-of-band. Design sketch:

- Agent calls something like `tclaude agent ask --timeout 20s
  --message "Spawn a reviewer agent in group foo?"` on the daemon.
- Daemon opens an approval popup (browser tab, see below) with three
  outcomes:
  - **ack** — keeps the popup open, cancels the auto-close timeout, no
    decision yet.
  - **approve** — returns success to the requesting agent.
  - **deny** — returns failure.
  - **timeout** — auto-close after N seconds (default 20s) returns
    failure (or "no decision", caller decides).
- Approval is logged so we can audit "who approved what when".

Implementation: the daemon already has an HTTP server on a Unix
socket; pair it with a small browser dashboard (see "Web dashboard"
below) and an ephemeral approval channel. For inspiration on the
popup/ack/timeout UX, see `/home/gigur/git/oh-shit-meeting` — that
project already implements browser-popup approval with these
semantics.

Open questions:
- One-shot grants vs. "remember this answer for N minutes" — useful
  for chatty agents but increases blast radius of a single approval.
- How are approval requests surfaced when no browser tab is open?
  Fall back to a desktop notification + reopening the dashboard?
- Should approvals carry the *full payload* (e.g. the proposed
  message body, the proposed group/member change) so the human can
  see what they're approving? Almost certainly yes.

### Popup transport hardening (residual /proc threat)

Today's approval popup security:

- 32-hex-char unguessable approval ID in the URL (bearer token).
- Loopback-only listener (127.0.0.1) with explicit RemoteAddr check.
- HttpOnly + SameSite=Strict session cookie set on first GET, required
  on POST (defense-in-depth against CSRF and scraped-URL replay).
- Origin / Referer must point at the popup base URL.

What's NOT closed: a same-user process can read
`/proc/<browser-launcher pid>/cmdline` to discover the popup URL,
issue a GET to receive the Set-Cookie, then POST `/approve/{id}/approve`
itself. The popup endpoints have no way to distinguish a browser
client from a curl-as-the-same-user attacker on a TCP socket — only
Unix sockets give us peer credentials, and browsers don't speak
those.

Same-user processes are already an implicit shared trust boundary
(an attacker with same-user privs can talk to `agentd.sock` directly
via peer creds), so the popup doesn't open a new gap — but it also
doesn't close the existing one. Future work to actually fix this:

- **Native dialogs.** Replace the loopback HTTP popup with platform
  dialogs (zenity / osascript / Win32 MessageBox). No URL exists to
  scrape. Loses the dashboard-reuse story (no shared port for the
  eventual GCP-IAM dashboard view), but the dashboard could keep
  loopback HTTP while approvals move out-of-band.
- **Tray-icon-mediated approve.** Pair the popup with the tray icon
  TODO: the popup's Approve/Deny buttons could *also* require a tray
  click within N seconds. Tray IPC is process-private to the daemon's
  GUI thread. Friction-heavier but raises the bar.
- **Don't pass URL via argv.** Launch the browser with a known
  origin and have the daemon hand the approval ID via a side channel
  the browser can fetch (e.g. a fixed welcome page that grabs a
  per-session ID via a cookie set on `127.0.0.1:<port>/`). Tricky:
  browsers still need *some* URL, and any URL has to land in argv
  somewhere. Marginal win.

### System tray icon — v2 follow-ups

V1 shipped (see DONE). Open follow-ups:

- **Yellow on pending approval** — flip icon to yellow while a
  `--ask-human` popup is awaiting decision; back to green on
  approve/deny/timeout.
- **Red on daemon down / shutting down**.
- **Flashing on unread inbox** — opt-in (loud).
- **Pending approvals submenu** — list waiting requests; click re-opens
  `/approve/{id}` (helps when the auto-opened tab got buried).
- **Tray-mediated approve** — pair with the popup so Approve/Deny also
  requires a tray click within N seconds (kills the residual /proc
  cmdline-scrape attack).
- **Focus dashboard tab on icon click** — same window-focus tricks the
  WSL notifications already use.

### Web dashboard (browser UI)

**v1 is shipped** — a read-only single-page dashboard served on the
same loopback port the approval popup uses. Tabs: Groups, Agents,
Permissions, Slug registry. Polls `/api/snapshot` every 5s. Auth
via per-process HttpOnly + SameSite=Strict cookie + Origin/Referer
pinned to the popup base URL (same threat model as the popup;
documented same-user /proc-leak limitation still applies).

Open it with `tclaude agent dashboard` (or `dashboard --print` to
just emit the URL). Daemon discovers the URL via `/v1/info`.

Pending follow-ups for v2+ (the GCP-IAM-style edits view):

**Multiple perspectives, switchable from the top nav.**

- **Groups view** — root list of groups; expanding a group shows its
  members with online indicator, alias/role/descr, and the group-
  scoped permissions each holds. Search at the top filters by group
  name / member alias / permission slug.
- **Agents view** — root list of conversations (members of any
  group + currently-online ones). Expanding an agent shows the
  groups it's in, its global permissions, and its per-group
  permission overrides. Same search box, scoped to the visible tree.
- **Permissions view** — invert the previous two: list of permission
  slugs, expanding shows every agent that holds it (globally or
  per-group). Useful for "who can spawn agents right now?".
- **Activity / inbox** — live list of agents (online/offline,
  current group, last activity, unread inbox count). Pending
  human-approval requests appear here with ack/approve/deny buttons
  (same UI as the standalone popup, just inline).

**Tree-style expand/collapse** for the first three views. Clicking a
node expands it, clicking again collapses. Hover/click on a permission
slug surfaces a tooltip/sidebar explaining what the slug authorises.

**Indicators alongside each row**:

- ● online / ○ offline
- ⚡ attached / ▷ active session in tmux
- inbox unread count
- count of granted permissions (so you can see at a glance who's
  privileged)

**Edits.** The dashboard should be the easiest place for the human to
grant/revoke permissions and group memberships. Buttons should call
the same daemon endpoints the CLI uses (`groups create|rm|add|remove
|update-member`, plus the new `permissions grant|revoke` once those
ship — see "CLI for permissions" below). Auditable: every mutation
shows up in a small activity log so the human knows what they
changed and when.

**Implementation:**

- v1 ships as static HTML+JS embedded via `//go:embed` (one HTML
  file, vanilla JS, polls `/api/snapshot` every 5s). Lightweight,
  no build step, ~290 lines.
- Reuses the loopback port the approval popup already binds. Pages
  fetch from `/v1/...` on the same origin (the daemon adds CORS
  scoping if needed; same-origin on loopback is the simplest option).
- Origin guard: only same-host. An ephemeral session cookie tied to
  the daemon's startup PID makes "another tab on the machine" attacks
  harder.

**Optional: framework migration.** Vanilla JS works for v1 but every
new feature (expand-state persistence, search, inline edits, "live"
activity tab) means hand-rolling DOM diffing and event delegation,
which adds up. **Consider migrating to React** (or Preact / Svelte)
when v2 lands — they'd give us:

- Built-in state preservation across re-renders (no more
  localStorage hacks for `<details>` open state).
- Cleaner edit forms (controlled inputs, validation, optimistic
  updates) for the inline grant/revoke + group mutators.
- Component-level diffing so polling updates don't blow away
  in-progress dialogs / search filters.

Trade-offs: a build step (vite + esbuild keeps it small), bigger
embedded asset, more JS to audit. Probably worth it once we cross
~5 features and ~700 lines of inline JS, and definitely worth it
before adding a search box + filtered tree views. Decide as part of
the v2 scope review; not a blocker.

Open questions:

- Should the dashboard run only on demand (`tclaude agentd ui` opens
  it on the existing loopback port) or always when the daemon is up?
  Probably always-on, since the approval popup is also served there
  and we already pay the bind cost.
- How much richness does the tree need? Start with collapse/expand,
  add filtering and column sorts only if it gets heavy.

### Delivery architecture (sandbox-aware)

**Problem:** when a sandboxed agent calls `tclaude agent message …`, the
DB write succeeds (because `~/.tclaude/db.sqlite` is allow-listed) but
the *tmux nudge* requires hitting `/tmp/tmux-…/tclaude` — a socket the
sender's sandbox typically blocks. The message is persisted but the
target sees nothing until they run `inbox ls` themselves. Same problem
applies to any cross-cutting concern (process-tree walks, lookups by
file path, etc.): they only work if the per-agent sandbox happens to
allow them.

The user-facing symptom is `(queued; target not online)` even when the
target's tmux session is very much alive.

**Three possible directions, in order of weight:**

1. **Hook-based lazy nudge (lightest).** Use the hook callback already
   wired up via `tclaude setup`. On any inbound hook
   (`PostToolUse`/`Notification`/etc.) the *receiver* checks for
   `agent_messages` rows where `to_conv = me` and `delivered_at IS NULL`,
   and the hook process (running in CC's environment, not the sender's
   sandbox) does the tmux send-keys to its own pane. No daemon.
   Latency = "next time the receiver does anything", which is usually
   sub-second. Best risk/reward for v1.

2. **`tclaude agentd` daemon.** A long-lived process started by
   `tclaude setup` (launchd on macOS, systemd user unit on Linux). Lives
   outside any agent sandbox. Watches `agent_messages.delivered_at IS
   NULL` (poll or SQLite hook), resolves target → tmux pane, sends the
   nudge, marks delivered. Could also handle: garbage-collecting dead
   session rows, refreshing tmux session names when CC restarts,
   exposing a richer query API. Cost: a new process to monitor, install,
   and reason about.

3. **Daemon over a Unix socket as the single agent API.** Instead of
   each `tclaude agent …` writing to SQLite directly, the CLI talks to
   the daemon over a socket, and the daemon owns DB + tmux + permission
   gating. Strongest authority story (the daemon decides who can talk to
   whom) but biggest rewrite — every existing agent CLI path goes
   through IPC. Aligns with "we can't always be aware of what sessions
   we're allowed to talk to": that lookup happens daemon-side, where it
   has the full picture.

**Decision:** foreground daemon, `tclaude agentd serve`. After a
discussion about `/fork` and inheritable env vars, the transport
pivoted from loopback HTTP+token to **HTTP over a Unix domain
socket** with **peer-cred identity** (no tokens). The daemon reads
the connecting peer's PID, walks to a `claude`/`node` ancestor, and
reads `~/.claude/sessions/<pid>.json` for the *current* conv-id —
which automatically tracks `/fork`/`/clear`/`/resume`. Full design
in [`agentd.md`](agentd.md).

**Status:** shipped in PR #47 (see DONE section below).

### Cross-machine (far future)

When/if we ever want to span hosts: federate `tclaude agentd` instances
over the network. Each host's daemon owns its local conv pool and proxies
messages destined for remote convs to the appropriate peer daemon. Keeps
the per-host peer-cred identity model intact. **Explicitly out of scope
for now** — single-host first.

### (legacy) Cross-machine
- For now everything is keyed off the local SQLite + filesystem inbox. A
  future variant could publish messages over the existing `git` sync
  channel (`pkg/claude/git`) so agents on different machines can talk.
- Likely needs a real message-id namespace (UUIDs) and conflict-free
  message ordering.

---

## DONE

Short notes only — see `docs/agent.md` and the code for details.

### PR #47 — v1 agent coordination + agentd (2026-05)

- `tclaude agent` CLI: `whoami`, `lookup`, `ls`, `message`, `groups
  create|rm|ls|members|add|remove`, `inbox ls|read`, `reply`.
- DB schema v8: `agent_groups`, `agent_group_members`, `agent_messages`.
- Tmux send-keys nudge when target online; queued otherwise.
- Group-shared enforcement — peers must share a group to message.
- Mutating-groups gate — refuses if a `claude`/`node` ancestor is
  found. Absolute (no `--allow-from-agent` shipped).
- `tclaude agentd serve` — Unix-socket HTTP, peer-cred identity.
- CLI requires daemon (no DB fallback).
- Skills bundled under `pkg/claude/agent/skills/`; installable via
  `tclaude setup --install-agent-skills`.

### Polish (post-#47, 2026-05)

- `pkg/claude/common/table` rendering across agent list views.
- `groups ls` MEMBERS + ONLINE columns.
- Groups column on `conv ls` / `conv ls -w`.
- ONLINE indicator on `agent ls` and `groups members`.
- `groups update-member` (alias/role/descr in place).
- Self-rename: `tclaude agent rename "<title>"`, slug `self.rename`,
  `requirePermission()` framework with config defaults + overrides.
- Group lifecycle Phase A: `groups stop` (soft `/exit`, `--force`
  kill-session) / `groups resume` (spawn detached `tclaude session
  new -r <conv> -d --global`). Slugs `groups.stop`/`groups.resume`.
- Browser dashboard v1 (read-only): Groups / Agents / Permissions /
  Slugs tabs, polls `/api/snapshot` every 5s, opens via
  `tclaude agent dashboard`.
- Multicast: `tclaude agent message group:<name> "..."` fan-out.
- User-facing docs: `docs/agent.md` + navbar entry.
- Permissions CLI + storage split: `tclaude agent permissions
  ls|grant|revoke|slugs`. Defaults in config.json; per-agent grants
  in SQLite (`agent_permissions`, schema v9). Effective set =
  `union(defaults, grants)`. Recursive: `permissions.grant|revoke`
  slugs gate the mutators.
- Agent state on dashboard (idle/working/awaiting/exited) mirroring
  `session/list.go` colours; `<details>` open state persisted in
  localStorage across polls.
- Shell autocompletions across `tclaude agent(d)` — group names,
  conv selectors (with title descriptions), permission slugs,
  message targets (`group:` prefix), inbox message IDs,
  `--ask-human` durations. Wired via boa
  `InitFuncCtx`+`SetAlternativesFunc`.
- System tray icon v1 (`fyne.io/systray`). Menu: Open dashboard,
  Reinstall agent skills, Open config.json, copy-paste rows for
  socket + popup URL, Quit. `--no-tray` opt-out for headless. Runs
  on main goroutine; signal/server-error/Quit converge on a single
  shutdown path. Linux/Windows pure-Go; macOS uses cgo (goreleaser
  splits builds: CGO_ENABLED=0 for linux/windows, =1 for darwin).
  Yellow/red/flashing indicators + pending-approvals submenu
  deferred to v2.
- `tclaude agent inbox sent` (outbox view). Lists this conv's
  outgoing messages with per-recipient delivery + read state.
  Backed by `db.ListAgentMessagesFromConv` + `/v1/inbox?outbox=1`.
