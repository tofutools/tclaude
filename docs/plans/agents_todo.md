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

### Group lifecycle
- `tclaude agent groups stop <group>` — gracefully end all sessions in a
  group. Open question: send `/exit` via tmux, or just kill the tmux
  sessions, or post a "wrap up" nudge and let agents finish.
- `tclaude agent groups archive <group>` — soft-delete (so message
  history stays queryable but membership is sealed).

### Discovery / state
- `tclaude agent groups ls --state=online|offline` — filter by whether
  members have a live tmux session right now.
- `tclaude agent ls --state=online|offline` — same filter, but for peers.
- Add a `groups` column to `tclaude conv ls` (and `-w` watch mode), so the
  user can search/sort/filter conversations by group and attach quickly.
  Probably needs a join in the conv list query plus an extra column in the
  bubbletea table. Useful even before all the messaging features are in
  daily use.
- Selectable filtering: pressing `g` in `conv ls -w` could open a fuzzy
  group picker.

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

### Web dashboard (browser UI)

A long-running browser view served by `tclaude agentd` (probably under
the existing Unix socket via a small reverse proxy, or just bind a
loopback HTTP port that the human can open). Renders:

- Live list of agents (online/offline, current group, last activity).
- Open inboxes per conversation.
- Pending approval requests with ack/approve/deny buttons (see HITL
  flow above).
- Group membership view + edit (human-only mutations).

Reuse `/home/gigur/git/oh-shit-meeting` patterns for popup/ack/timeout
behaviour. The same UI doubles as the approval frontend, so we don't
need a separate popup library.

Open questions:
- Auth on a loopback port — do we need anything beyond "must be on
  this machine"? Probably yes (same-origin attacks via other browser
  tabs); some kind of ephemeral token per session opens.
- Should the dashboard run only on demand (`tclaude agentd ui`) or
  always when the daemon is up?

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
- **Skill bundled** at `pkg/claude/agent/skills/agent-coord/SKILL.md`;
  installable via `tclaude setup --install-agent-skill`.
