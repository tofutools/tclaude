# Agent coordination — TODO / DONE

Living todo list for agent coordination work in tclaude. Update as items
ship or get scoped out. The detailed v1 design lives in
[`agent-coord.md`](agent-coord.md).

---

## In progress

(nothing currently being worked on — pick from TODO below)

---

## NEXT (top priority)

### Agent reincarnate (replaces /clear for self-lifecycle)

Why: CC's `/clear` rotates the conv ID, which orphans the agent's
name, per-conv permission grants, and group memberships. We learned
this the hard way after wiring `tclaude agent clear` to inject
`/clear` — the agent loses its identity the moment it clears, which
is almost never what a long-running agent wants. /compact preserves
conv ID and is safe; /clear isn't.

Replacement design: `tclaude agent reincarnate [follow-up]`. The
daemon does NOT call `/clear`. Instead it:

1. Snapshots the calling agent's state from SQLite: group memberships
   (with alias/role/descr per group), permission grants, group
   ownerships, cwd from the sessions row.
2. Spawns a fresh tclaude session in the same cwd
   (`tclaude session new -d --global -C <cwd>`) and polls the
   sessions table until the new conv-id materialises (same pattern
   `groups spawn` already uses).
3. Migrates the snapshotted state to the new conv-id: insert new
   rows for the new conv, delete the old rows. Single transaction
   so a partial failure doesn't leave both copies live.
4. If a follow-up was provided: enqueue it as an `agent_messages`
   row addressed to the new conv. The new agent's first turn will
   see a `[system: new agent message #N for you. fetch with: tclaude
   agent inbox read N]` nudge once it comes online and the flush
   pipeline kicks in. (More reliable than tmux send-keys racing
   against the new pane's startup.)
5. Soft-stops the old conv by injecting `/exit` into its tmux pane
   (same pattern as `groups stop` soft mode). Old CC closes cleanly;
   the old tmux session goes away.
6. Returns the new conv-id, label, and tmux session name to the
   caller so the human (or whatever scripted the call) can attach.

Permission slug: `self.reincarnate`. **Default: granted** — alongside
`self.rename` and `self.compact`. Identity is preserved (groups,
permissions, ownership migrate to the new conv), so the blast radius
is bounded: a fresh CC instance gets started, the old one ends. We're
trying it as a default-on capability and will tighten if we see
abuse.

Continuity is the project's responsibility, not the daemon's:
- The agent must persist work-in-progress (decisions, plan, partial
  results, file paths, next step) to disk *before* calling
  reincarnate — typically a notes file or a TODO doc inside the
  project being worked on. The daemon only migrates *identity*, not
  *task state*.
- The project's CLAUDE.md (or equivalent) should document where the
  agent writes its progress, and how a freshly-reincarnated agent
  reloads enough to continue. The skill points to this convention
  but doesn't enforce it.

What we don't migrate (v1):
- CC's conversation title (set via `/rename` inside CC). Not in our
  DB. The new agent can self-rename in its follow-up if it cares.
- CC's actual message history. That's the whole point — fresh
  context.

Removed: `tclaude agent clear` and the `self.clear` permission slug.
The literal `/clear` path is broken for any agent in a group or
holding per-conv permissions (it orphans both), and reincarnate
covers the legitimate use case. If a human really wants to inject
`/clear` into a CC pane they can type it themselves.

Follow-up improvements (separate items):
- ~~**Auto-detach-and-reattach the human's terminal.**~~ **Shipped
  + verified (test #2, 2026-05).** Daemon runs `tmux list-clients
  -t <old>` and `tmux switch-client -c <tty> -t <new>` for each
  client right before injecting `/exit` on the old pane. Carry-over
  count surfaced in the response as `switched_clients`. End-to-end
  test from an attached terminal confirmed: terminal lands on the
  new pane without a manual attach, and the follow-up nudge
  arrives in the now-attached pane.
- ~~**Investigate: stale terminal title after switch-client.**~~
  **Closed — not a bug (test #2).** Empirically the title DID
  refresh to the new session label after `switch-client`, so the
  WSL title-based notification path kept working. The earlier
  worry that the wrapper's `setTerminalTitle("tclaude:<label>")`
  (`pkg/claude/session/attach.go:90`) would freeze the title was
  unfounded — tmux owns the title per-session and refreshes it on
  switch. No daemon-side OSC rewrite needed.
- ~~**Investigate: env coherence with the wrapper.**~~ **Closed
  — verified clean (test #2).** From inside the freshly-switched
  pane: `echo $TCLAUDE_SESSION_ID` returned the new label,
  `tclaude agent whoami` resolved to the new conv-id, and
  `tclaude session ls` showed the new session as the active row.
  The wrapper's stale `TCLAUDE_SESSION_ID` in its blocked-on-tmux
  process env doesn't leak — tmux session env (baked at
  `session/new.go:160`, propagated by `tmux_keys.go:33`) is the
  canonical source for everything that runs inside the new pane.
- **Heavier alternative if a regression appears:** IPC-signal the
  foreground `tclaude attach` process to kill its tmux subprocess
  and exec into a fresh `tclaude session attach <new-label>`. Not
  needed today; keep on the shelf in case the cheap path breaks.
- Optional title preservation if we wire CC's title into our DB
  (e.g. via a hook that captures it, or by parsing CC's conv jsonl).

### Agent clone — shipped (2026-05)

`tclaude agent clone [follow-up] [--no-copy-conv] [--target <peer>]`
ships. Reuses reincarnate's snapshot pass; differs in three ways:

- **Identity copied, not migrated.** Original keeps every group
  membership, permission grant, and ownership. Clone gets a copy
  with a `-clone` alias suffix per group.
- **Conv jsonl copy is the default.** Uses `convops.CopyConversationToPath`
  to fork the existing conversation history onto a freshly-minted
  conv-id, then `tclaude session new -r <new-conv>` so CC loads
  the cloned conversation. `--no-copy-conv` flips to blank context.
- **No `/exit` on the original.** Both are running after the call.

Slugs: `self.clone` (default-granted alongside `self.compact` /
`self.reincarnate` via `tclaude setup --install-default-agent-permissions`)
and `agent.clone` (default human-only; cross-agent / manager
pattern). Both routed through the existing
`runCloneOrchestration` helper for the self/cross split.

#### Open follow-ups

- ~~**Alias scheme: always `-clone-<N>`.**~~ **Shipped.** Every
  clone gets `<base>-clone-<N>` (or `clone-<N>` when the original
  had no alias), where N is the smallest integer free across all
  group_member rows system-wide. Clone-of-a-clone strips the
  existing `-clone-<digits>` suffix before recomputing, so
  `worker-clone-3` clones to `worker-clone-N` (bumped) rather than
  `worker-clone-3-clone-1` (nested). Counter is global, not per-
  group, so the same clone gets the same alias across every group
  it inherits.
- **Rate limiting.** A runaway loop can fork unboundedly since the
  original isn't taken down. Worth adding 1-clone-per-minute at the
  daemon if it shows up in practice.
- **--no-copy-conv polish.** Today the no-copy path uses the same
  poll-for-new-conv-id loop as reincarnate; CC has to mint the
  conv-id before identity can be copied. Hopefully fast enough; if
  it ever grows slow, consider pre-seeding a placeholder row.

### Cross-agent lifecycle (manager pattern)

The **manager pattern**: an elevated agent (or group owner) can act
on *other* agents — typical use is a manager watching workers,
reincarnating ones whose context has rotted with a follow-up
pointing them at the next batch of work.

Permission model:

- `self.<verb>` — operate on yourself only. (Today: `self.rename`,
  `self.compact`, `self.reincarnate`.)
- `agent.<verb>` — operate on *another* agent (target specified by
  conv-id / alias / selector). Default: human-only. Granted to a
  manager agent explicitly.
- **Group ownership grants implicit power.** A group owner can call
  any `agent.<verb>` against any member of a group they own without
  needing the slug. Mirrors how `member.add` / `member.remove`
  already special-case owners; concretely powered by
  `ownerOfGroupContaining(caller, target)` in
  `pkg/claude/agentd/agent_dispatch.go`.

Endpoints follow `/v1/agent/{selector}/{verb}`. The dispatcher
resolves the selector via `agent.ResolveSelector`, then routes to
the per-verb handler which calls `requireCrossAgentPermission` and
runs the same orchestration with the target conv as the subject.

Audit: cross-agent migrations record `granted_by` as
`system:reincarnate:by=<caller-conv>` (vs plain `system:reincarnate`
for self), so "who killed my agent" forensics work from the
agent_permissions / agent_group_owners audit columns alone.

#### Shipped (2026-05)

- ~~`agent.reincarnate` slug + `/v1/agent/{conv}/reincarnate`
  endpoint~~. Reincarnate orchestration extracted into a shared
  helper (`runReincarnationOrchestration`) so self and cross-agent
  paths share the same migration / spawn / soft-stop logic.
- ~~Group-owner implicit power~~ via
  `ownerOfGroupContaining`.
- ~~Handoff message FromConv set to the caller~~, so the new agent
  sees who asked it to pick up the work and can reply directly.
- ~~CLI `--target` flag~~ on `tclaude agent reincarnate`. Empty →
  self path; non-empty → cross-agent path.
- Skill (`agent-lifecycle`) updated with the manager-pattern
  section.

#### Shipped also (2026-05)

- ~~`agent.compact`~~ + `tclaude agent compact --target` CLI.
  Mirrors `agent.reincarnate`: same dispatcher, same auth model
  (slug OR owner-of-group), reuses `injectSlashCommand` on the
  target's pane. Self/cross paths share `runSlashOrchestration`.
  Slug `agent.compact`, default human-only.
- ~~`agent.rename`~~ + `tclaude agent rename --target` CLI. Same
  shape; charset gate hoisted into a shared
  `runRenameOrchestration` helper used by both self and cross
  paths. Slug `agent.rename`, default human-only.
- ~~`agent.clone`~~ + `tclaude agent clone --target` CLI. Routed
  through `runCloneOrchestration` (the same body the self path
  uses). Slug `agent.clone`, default human-only.

#### Follow-ups (still TODO)

- **X-Tclaude-Ask-Human on cross-agent endpoints.** Today
  `requireCrossAgentPermission` doesn't honor the popup header
  (manager pattern is opt-in via explicit grants). Re-evaluate if a
  use case appears — e.g. a manager that wants to act on a peer it
  doesn't normally manage.
- **Open question — orthogonal vs. implication.** Today
  `agent.<verb>` and `self.<verb>` are orthogonal (granting one
  doesn't grant the other). Keeping them split for now; revisit if
  it turns out managers always also want self-management.

---

## TODO

### Session shortcuts
- ~~Spawn-and-join in one command.~~ **Shipped** as
  `tclaude agent spawn <group> [--alias …] [--role …] [--descr …]
  [-C cwd]`. Daemon orchestrates: forks `tclaude session new -d
  --global --label <random>`, polls SQLite for the new conv-id, then
  adds it to the group. Permission slug `groups.spawn` (default:
  human-only). Returns the attach command for the new tmux session.
- Variant: `tclaude --join-group <group>` flag on the top-level
  command (so `tclaude` itself starts an attached session that
  auto-joins). Less useful than the daemon-orchestrated `agent spawn`
  for parallel work, but still nice for the "I want to attach right
  now" path. Open question: pre-launch (DB write before conv-id) vs.
  first-tick (after the conv-id is known via a hook).

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
- ~~In-Reply-To threading.~~ **Shipped.** `parent_id` column on
  `agent_messages` (schema v10), auto-set on `reply`. `inbox read`
  renders `In-Reply-To: <id> ("Subject")`. `inbox ls` prefixes
  reply rows with `↳`. `parent_id` surfaced in `/v1/inbox` rows so
  the dashboard can render thread arrows in v2.
- ~~Flush undelivered nudges when a conv comes back online.~~
  **Shipped.** Identity middleware kicks a debounced (5s/conv)
  background flush whenever it resolves a peer's conv-id. The
  flush walks `delivered_at = ''` rows for that recipient,
  atomically claims each one, and sends the bracketed nudge.
  Concurrent flushes are race-free via `db.ClaimAgentMessageDelivery`.
- ~~`tclaude agent inbox prune --older-than 30d --read-only`~~ —
  **shipped.** Required `--older-than` accepts time.ParseDuration
  values plus `Nd`/`Nw`. `--read-only` restricts to messages the
  recipient has read. Caller-scoped: only deletes rows where
  from_conv or to_conv equals the calling agent's conv-id.

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
  name / member alias / permission slug. ~~Owner rendering~~
  **shipped (2026-05)**: members who are also owners get an "owner"
  badge in the role column; pure-owners surface as their own rows.
  Mirrors the CLI convention (`groups members`).
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

**Direct-manipulation interactions in the Groups view** (the natural
home for membership editing):

- **Drag-and-drop members between groups.** Drag a member row from
  group A and drop it on group B's header → moves the conv (POST add
  to B + DELETE from A, in that order so the conv is never groupless
  mid-drag). Drop targets pulse on hover so the action is
  discoverable.
- **Ctrl+drag = copy/multi-membership.** Holding Ctrl while dropping
  pops a small choice menu:
   1. Add to the new group (dual membership; the conv is now in
      both A and B).
   2. Clone the agent and add the clone to B (uses `agent clone`
      once that ships; the original stays in A untouched, the
      clone joins B).
   3. (open question — better idea?) Maybe "spawn a fresh
      sibling into B" as a third option, useful when copy-the-conv
      isn't what you want but you do want a peer in the same role.
  Default action without Ctrl is the move (current behaviour with
  drop targets).
- **Per-member action buttons.** Far-right (or far-left) cell on
  each member row gets icon buttons:
   - Toggle owner: grant-owner / revoke-owner depending on current
     state. Uses `groups grant-owner` / `revoke-owner`.
   - Remove from group: confirmation modal (destructive), calls
     `groups remove`.
   - Possible third: reincarnate / compact (manager-pattern
     verbs) once we want one-click lifecycle controls. Gated by
     the same `agent.<verb>` slug + owner-of-group rules.
- **Add-member affordance in the group header.** A "+" button next
  to the group name opens a search overlay/popup listing
  candidates. Defaults to the agent set (members of any group +
  online conv-sessions); a "include all conversations" checkbox
  expands the list to every conv-id we know about. Selecting one
  calls `groups add` against the current group.

Implementation notes for the interaction layer:

- Drag/drop is the natural moment to consider a real framework.
  Vanilla DnD with HTML5 drag events works but needs careful
  ghost-image handling and drop-zone hover state. React-DnD or
  Solid's reactive model would carry more weight as edit features
  grow.
- The action buttons need a confirmation modal pattern (at least
  for "remove from group" and "revoke owner" — anything that loses
  state). Same pattern can serve the future "permissions revoke"
  action in the Permissions view.

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

### Agent self-lifecycle (2026-05)

- `tclaude agent compact [follow-up]` — daemon injects `/compact` into
  caller's pane; optional follow-up queues as next prompt. Slug
  `self.compact`.
- `tclaude agent clear [follow-up]` — same with `/clear`. Slug
  `self.clear`.
- `tclaude agent context-info` — reads `sessions.context_pct` +
  `compact_pending`. No slug (read-only).
- New `agent-lifecycle` skill with thresholds (~50% on 1M context, ~75%
  on 200k) and the "keep a navigable index, don't reload massive
  context after compact" pattern.

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
- `--state=online|offline` filter on `agent ls` and `agent groups ls`.
  Tab-completion offers the two values with descriptions.
- `tclaude agent inbox prune --older-than <dur> [--read-only]` —
  caller-scoped delete of old `agent_messages` rows. Accepts day/
  week suffixes. Backed by `db.PruneAgentMessagesForConv` +
  `/v1/inbox/prune`.
- Message threading (schema v10). `agent_messages.parent_id`
  auto-set by `reply`. `inbox read` shows `In-Reply-To: <id>
  ("subject")`. `inbox ls` prefixes replies with `↳`.
- Flush-on-online: identity middleware kicks a debounced background
  flush of any `delivered_at = ''` rows whenever a peer's conv-id
  resolves. Race-free via `ClaimAgentMessageDelivery` (atomic
  UPDATE..WHERE delivered_at = ''). Tested with concurrent flushers.
- `tclaude agent spawn <group>`: fresh CC session + auto-join. Daemon
  forks `session new -d --global --label <random>`, polls SQLite for
  the new conv-id, then registers it in the group with optional
  alias/role/descr. Slug `groups.spawn` (human-only by default).
- Lookup fallback to `agent_group_members` for fresh-spawned convs
  and per-group aliases. Existing refresh-on-miss still fires when
  both conv_index and members miss.
- Group owners (schema v11). `agent_group_owners` table; owners can
  message a group's members and multicast without being members.
  CLI: `groups owners`, `grant-owner`, `revoke-owner`. Slug
  `groups.own` (human-only by default). `groups members` shows
  `(owner)` tag for member-owners; pure-owners surface as their own
  rows with role=owner. Reply path no longer requires shared-group
  — if you received a message you can reply, even out-of-group.
  Auto-own-on-create: an agent that creates a group becomes its
  owner automatically (skipped for human creator since humans bypass
  the permission system).
