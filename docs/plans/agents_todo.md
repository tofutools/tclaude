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

- `tclaude agent groups stop <group>` — gracefully end every member's
  session. Implementation choices, in order of weight:
  - **Soft**: post a "wrap up" nudge per member, mark group as
    `stopping`, let agents `/exit` themselves.
  - **Medium**: send `/exit` via tmux send-keys to each member's pane
    (mirrors the inject mechanic agentd already uses for `/rename`).
    Bounded wait, then escalate.
  - **Hard**: tmux kill-session. Last resort; risks losing
    unsubmitted input.
  Probably ship soft+hard with a `--force` toggle. Membership is
  preserved (so `resume` can bring everyone back).

- `tclaude agent groups resume <group>` — sugar over `spawn`: re-attach
  any members that have a conv but no live session. Distinct from
  `spawn` only in error semantics — `resume` errors if any member is
  in the no-conv-yet state, since "resume" implies prior existence.

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
- **Multicast / group broadcast.** Send one message to every member of
  a group. Two reasonable shapes:
  - `tclaude agent message group:<name> "..."` — selector prefix
    `group:` triggers fan-out.
  - `tclaude agent broadcast <group> "..."` — explicit verb.
  Implementation: daemon inserts one row per recipient (skipping the
  sender), nudges only live tmux panes that aren't the sender's. The
  sender's row stays out of their own inbox (we don't echo). Replies
  go back to the sender as a normal direct message; "reply-all" is a
  follow-up.

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
- Each conv now has an implicit inbox (rows in `agent_messages` where
  `to_conv = me`). `tclaude agent inbox ls` and `inbox read <id>` are the
  v1 readers. Conversations also keep an outbox view of their own sent
  messages — currently only via direct DB query; surface as
  `inbox sent`.
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

### System tray icon

A long-running tray icon for `tclaude agentd` so the human can see at
a glance whether the daemon is up, and reach the dashboard / common
actions in one click. Inspired directly by `/home/gigur/git/oh-shit-meeting`'s
systray (uses `fyne.io/systray`, pure-Go on Windows, gracefully no-ops
on hosts without a tray host like WSL or some GNOME setups).

Indicators (icon colour or overlay):

- **Green** — daemon up, no pending work.
- **Yellow** — daemon up, **pending human approval popup** (the
  approval flow we just shipped). Same idea as oh-shit-meeting's
  yellow-when-action-needed.
- **Red** — daemon down (or about to go down).
- **Flashing** — unread agent inbox messages on any conv (configurable
  threshold; flash is loud, so probably opt-in).

Menu items:

- **Open dashboard** → opens `popupBaseURL/` (the loopback HTTP root)
  in the browser. Same `openBrowser` helper popup.go already has.
- **Pending approvals (N)** — submenu listing currently-waiting
  approval requests; clicking one re-opens its `/approve/{id}` page.
  Useful when the auto-opened browser tab got buried.
- **Reinstall agent skills** → runs the same code path as
  `tclaude setup --install-agent-skills` so the human can refresh
  bundled skills after a `go install …@latest` without dropping to a
  shell.
- **Open ~/.tclaude/config.json** → launch `xdg-open` /
  `open` / `start` on the config file. Convenient for editing
  permissions until the dashboard's edit UI lands.
- **Show socket / popup port info** — small disabled menu items that
  show `agentd.sock` path and the popup base URL, for copy-paste.
- **Quit** → graceful daemon shutdown (the existing SIGTERM path).

Implementation notes:

- The `agentd serve` process runs the tray loop on its main goroutine
  (systray needs the main thread). The HTTP servers move to
  goroutines (they already are).
- Cross-platform: macOS/Windows have native trays;
  `fyne.io/systray` works on both. Linux varies — works on Plasma /
  most XFCE / some GNOME, no-ops on WSL or pure Wayland sessions.
  Document the support matrix.
- Add a `--no-tray` flag to `tclaude agentd serve` for environments
  where the tray dep is undesired (CI, headless Docker, etc.). Tray
  is opt-out, not opt-in, since the whole point is "daemon visible
  by default."
- Optional bonus: tray click doubles as "focus most-recent dashboard
  tab" — same window-focus tricks the WSL notifications already use.

### Web dashboard (browser UI)

A long-running browser view served by `tclaude agentd` on the same
loopback port the approval popup uses (or a separate one). Goal: a
GCP-IAM-style "who can do what to which resource" overview, plus
live agent activity. Renders:

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

- Static HTML+JS page served by the daemon (no SPA framework
  necessary — htmx or vanilla JS keeps it lightweight).
- Reuses the loopback port the approval popup already binds. Pages
  fetch from `/v1/...` on the same origin (the daemon adds CORS
  scoping if needed; same-origin on loopback is the simplest option).
- Origin guard: only same-host. An ephemeral session cookie tied to
  the daemon's startup PID makes "another tab on the machine" attacks
  harder.

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

### PR #47 — v1 agent coordination + agentd (2026-05)

- **`tclaude agent` CLI**
  - `whoami`, `lookup <name>`, `ls`
  - `message <target> <body>` (with `--stdin` / `--file`, optional `--subject`)
  - `groups create|rm|ls|members|add|remove`
  - `inbox ls|read` (with `mailbox`/`mail` aliases)
  - `reply <id>` — looks up sender from message, inherits `Re: <subject>`
- **DB schema v8** — tables `agent_groups`, `agent_group_members`,
  `agent_messages` (`from_conv`, `to_conv`, `subject`, `body`,
  `created_at`, `delivered_at`, `read_at`).
- **Tmux nudge** via `send-keys` when target session is alive; queued
  otherwise.
- **Group-shared enforcement** — daemon refuses messages between peers
  who don't share a group.
- **Mutating-groups gate** — daemon walks PID tree; refuses
  `groups create|rm|add|remove` if a `claude`/`node` ancestor is found.
  (Note: the originally-planned `agent.allow_agent_mutate_groups` config
  override and `--allow-from-agent` flag were not shipped — gate is
  absolute. Re-evaluate if we want agents to self-onboard.)
- **`tclaude agentd serve`** daemon — foreground HTTP over Unix domain
  socket with peer-cred identity (no tokens). Reads peer PID, walks to
  `claude`/`node` ancestor, reads `~/.claude/sessions/<pid>.json` for
  current conv-id (tracks `/fork`/`/clear`/`/resume`).
- **CLI requires daemon** — `tclaude agent …` no longer falls back to
  direct DB access; refuses loudly if `agentd` isn't running.
- **Skills bundled** under `pkg/claude/agent/skills/<name>/SKILL.md`;
  installable via `tclaude setup --install-agent-skills`.

### Polish (post-#47, 2026-05)

- **Common-table rendering for agent CLI lists.** `agent ls`,
  `groups ls`, `groups members`, and `inbox ls` all render via
  `pkg/claude/common/table` instead of ad-hoc `fmt.Fprintf`. JSON
  output unchanged.
- **Online member counts.** `groups ls` shows `MEMBERS` and `ONLINE`
  side by side. `isConvOnline` factored out of `nudgeIfAlive`.
- **Groups column on `conv ls` / `conv ls -w`.** Both list views grow
  a "Groups" column when at least one conv is in any group, so you
  can see group membership while picking a conversation. Backed by
  `db.GroupNamesByConv()`.
- **Per-row ONLINE indicator.** `agent ls` and `groups members` now
  show a leading ● glyph for peers with a live tmux session.
- **`groups update-member`.** Redesignate alias/role/descr in place.
  Same human-only gate as add/remove. Empty string clears a field.
- **Self-rename via agentd + permission framework.** `tclaude agent
  rename "<title>"` injects `/rename <title>` into the caller's own CC
  pane via tmux send-keys. Permission-gated on `self.rename`. The
  daemon ships a `requirePermission()` helper backed by an `agent`
  section in `~/.tclaude/config.json` (defaults +
  per-conv-id/prefix/title overrides). Humans bypass the gate. Skill
  documents the command + how to grant the permission.
- **User-facing docs.** `docs/agent.md` covers the agent feature
  end-to-end: identity model, command reference, permission model
  with the storage split (config defaults vs SQLite per-agent
  grants), `--ask-human` popup flow, bundled skills,
  troubleshooting. Linked from `docs/index.md`'s navbar and Features
  list. Design docs under `docs/plans/` stay where they are.
- **Permissions CLI + storage split.** `tclaude agent permissions
  ls|grant|revoke|slugs` — inspect and edit agent permissions without
  hand-editing `~/.tclaude/config.json`. Targets are the magic word
  `default` (the global defaults list, in config.json) or a conv
  selector (UUID, prefix, title — resolves to the full conv-id). Per-
  agent grants live in **SQLite** (table `agent_permissions`, schema
  v9) — only the global defaults stay in config.json. Per-agent grants
  ADD to defaults rather than replace them, so an agent's effective
  set is `union(defaults, grants)`. Backed by daemon endpoints
  `/v1/permissions[/slugs|/grant|/revoke]` and a single source-of-truth
  slug registry (`self.rename`, `groups.create|rm`,
  `member.add|remove|redesignate`, `permissions.grant|revoke`).
  `grant` refuses unknown slugs. The `permissions.grant|revoke` slugs
  are themselves permission-gated, so the framework is recursive —
  humans bypass by default; no agent holds them out of the box.
