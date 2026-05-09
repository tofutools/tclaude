# Agent Coordination 🤝

> **BETA / EXPERIMENTAL**
>
> The agent feature is under active development. Commands, flags,
> permission slugs, the daemon wire format, and the SQLite schema may
> all change without notice. Per-agent permission grants are stored in
> a v9 schema table (`agent_permissions`) which has not been
> field-tested across upgrades. **Don't build automation against this
> yet that you can't readily migrate.** See
> [`docs/plans/agents_todo.md`](plans/agents_todo.md) for what's still
> in flight.

Cross-session messaging and self-service capabilities between Claude
Code conversations on the same machine, gated by a permission model the
human curates.

`tclaude agent` (the CLI) talks to `tclaude agentd` (a long-running
daemon) over a Unix socket. The daemon owns the database, tmux nudges,
and permission gating; the CLI is a thin client. Identity is derived
from the connecting socket peer's PID — no tokens to manage, and
`/fork` keeps working because the daemon re-reads each caller's
current conv-id on every request.

## What you can do

- **Talk between conversations.** Send messages, replies, and
  threaded follow-ups between CC sessions. The receiver gets a tmux
  nudge if they're online; otherwise the message queues in their
  inbox.
- **Group sessions.** Allow-list who can talk to whom. Out-of-group
  messages are refused server-side.
- **Delegate capabilities.** Per-agent permissions live in SQLite;
  you (the human) decide which agents can rename themselves, manage
  group membership, etc. Agents can also ask for one-off escalation
  via a browser approval popup.

## Prerequisites

- **`tclaude setup`** — registers hooks and the agentd socket path.
- **`tclaude setup --install-agent-skills`** — materialises the
  bundled agent skills under `~/.claude/skills/`. Without these
  skills installed, agents won't know to use these commands.
- **`tclaude agentd serve`** — running in a non-sandboxed shell. The
  CLI refuses to fall back to direct DB access when the daemon is
  down — that's deliberate, so the auth model can't be bypassed by
  killing the daemon.

The daemon binds two sockets:

- `~/.tclaude/agentd.sock` — Unix socket for `tclaude agent` traffic.
- `127.0.0.1:<random>` — loopback HTTP for the human-approval popup
  (only used when an agent passes `--ask-human`).

## Quick start

```bash
# 1. Run the daemon in a separate (non-sandboxed) terminal.
tclaude agentd serve

# 2. Install the agent skills so CC agents know about these commands.
tclaude setup --install-agent-skills

# 3. As the human, set up a group and add some sessions.
tclaude agent groups create reviewer-team --descr "code review crew"
tclaude agent groups add reviewer-team <conv1> --alias lead   --role tech-lead
tclaude agent groups add reviewer-team <conv2> --alias tester --role test-runner

# 4. From inside a CC session, an agent can now reach peers.
tclaude agent ls
tclaude agent message lead "Can you review PR #42?"
```

## Identity

Every command is associated with a *caller identity*:

- **Human** — invocation has no `claude`/`node` ancestor in its
  process tree. Bypasses every permission gate.
- **Agent** — invocation came from inside a CC session. The daemon
  walks up to the nearest claude/node ancestor and reads
  `~/.claude/sessions/<pid>.json` for its current conv-id. That
  conv-id is the agent's identity.

`tclaude agent whoami` prints whichever the daemon resolved.

## Commands

### whoami / lookup / ls

```bash
tclaude agent whoami           # who am I (or <human>)
tclaude agent lookup <name>    # resolve alias / prefix / title to a full conv-id
tclaude agent ls               # peers in any group I'm in (online indicator + groups)
tclaude agent ls --json
```

`lookup` accepts a UUID, an 8-char prefix, or a current display title.
`ls` is restricted to peers reachable through a shared group — the
group acts as an allow-list.

### message / reply

```bash
# direct
tclaude agent message <peer> "your message text"
tclaude agent message <peer> --subject "ack" --stdin <<EOF
multi-line body
EOF
tclaude agent message <peer> --file plan.md

# broadcast to every member of a group except yourself
tclaude agent message group:reviewer-team "PR #42 ready for eyes"

# reply (looks up the sender from the original message id; no need to
# copy conv-ids out of the headers)
tclaude agent reply <id> "got it"
tclaude agent reply <id> --subject "Re: not the default" "..."
```

For direct messages the sender and target must share a group,
otherwise the daemon refuses with `not in a shared group`. For
multicast (`group:<name>` target), the sender must be a member of
that group. If the target's tmux session is alive they get a
system-style nudge; otherwise the message queues in their inbox
until they `inbox ls`. Replies to a multicast come back as normal
direct messages — there is no automatic "reply-all".

### inbox

```bash
tclaude agent inbox ls                # last 20, all
tclaude agent inbox ls --unread       # only unread
tclaude agent inbox ls --limit 100
tclaude agent inbox read <id>         # marks read; --keep-unread to defer
tclaude agent inbox read <id> --json
```

`inbox` has aliases `mailbox` / `mail`. Reading a message returns
RFC-822-shaped headers — `From`, `To`, `Group`, `Subject`, `Date`,
`Reply-To`, `Reply-Cmd` — followed by the body.

### groups

```bash
tclaude agent groups ls                                   # all groups + member/online counts
tclaude agent groups members <group>                      # members + ● online indicator
tclaude agent groups create <group> [--descr "..."]
tclaude agent groups rm <group>                           # fails if any messages reference it
tclaude agent groups add <group> <conv> [--alias N --role R --descr T]
tclaude agent groups remove <group> <conv>
tclaude agent groups update-member <group> <conv> [--alias N --role R --descr T]
```

`update-member` only touches the flags you pass; pass `--alias=` (an
explicit empty string) to clear a field. All mutating subcommands
take `--ask-human <duration>` (see below).

### permissions

```bash
tclaude agent permissions slugs                          # registry of known slugs + descriptions
tclaude agent permissions ls                             # defaults + per-agent grants
tclaude agent permissions ls <conv|alias>                # effective set for one agent
tclaude agent permissions ls default                     # defaults only
tclaude agent permissions grant default <slug>           # add to global defaults
tclaude agent permissions grant <conv|alias> <slug>      # add per-agent grant
tclaude agent permissions revoke default <slug>
tclaude agent permissions revoke <conv|alias> <slug>
```

See **Permission model** below for the full picture.

### rename

```bash
tclaude agent rename "<title>"        # caller renames its own conversation
```

Mechanic: the daemon injects `/rename <title>` into the caller's CC
pane via `tmux send-keys`. Gated on the `self.rename` permission.

Title charset is strict (`[A-Za-z0-9_\-\[\]{}() ]`, single ASCII
spaces, max 64 chars). This is a hard security gate — the title
becomes literal keystroke input, so anything weird in it could
inject other slash commands.

## Permission model

Every mutating action is gated by a *permission slug*. The daemon
checks the caller's identity:

1. **Human?** Pass.
2. **Agent?** Allowed iff the slug is in either `default_permissions`
   (global) or the agent's per-conv grants (SQLite).

### Storage split

| Where                                     | What                          | How to edit               |
|-------------------------------------------|-------------------------------|----------------------------|
| `~/.tclaude/config.json` → `agent.default_permissions` | Slugs granted to **every** agent | hand-edit, or `permissions grant default <slug>` |
| SQLite `agent_permissions` table          | Per-conv grants (additive on top of defaults) | `permissions grant <conv> <slug>` (writes the DB row) |

An agent's effective permission set is `union(defaults, grants)`.

### Slugs

| Slug                    | Allows                                                    |
|-------------------------|-----------------------------------------------------------|
| `self.rename`           | `tclaude agent rename` (calls `/rename` in own pane)      |
| `groups.create`         | `tclaude agent groups create`                             |
| `groups.rm`             | `tclaude agent groups rm`                                 |
| `member.add`            | `tclaude agent groups add`                                |
| `member.remove`         | `tclaude agent groups remove`                             |
| `member.redesignate`    | `tclaude agent groups update-member`                      |
| `permissions.grant`     | `tclaude agent permissions grant`                         |
| `permissions.revoke`    | `tclaude agent permissions revoke`                        |

Slugs not in this list are accepted by the daemon at evaluation time
(forward-compat for slugs a future build will wire up), but
`permissions grant` refuses them at the CLI to catch typos.

Run `tclaude agent permissions slugs` for the live registry.

### Ad-hoc human approval (`--ask-human`)

Every mutating command takes `--ask-human <duration>` (e.g. `30s`,
`2m`, or a bare integer for seconds; capped at 300s). On permission
denial, the daemon opens a browser popup with **Approve / Deny /
+5min** buttons:

```bash
tclaude agent groups create foo --ask-human 30s
# → CLI prints "Waiting up to 30s for human approval..."
# → browser popup opens, shows the requester / target / body
# → human clicks Approve → CLI proceeds
# → human clicks Deny or timeout fires → CLI fails with 403
```

**Timeout = Deny** so an unattended popup never silently grants. The
popup is loopback-only and authenticated by an HttpOnly session
cookie + Origin/Referer matching, but a same-user process can still
read the URL out of `/proc` — this matches the existing same-user
trust boundary at `agentd.sock`. (Future work in
`docs/plans/agents_todo.md`.)

### Recursion

`permissions.grant` and `permissions.revoke` are themselves slugs.
That means `permissions grant` / `revoke` are gated like every other
mutator: the human can run them by default; an agent can only run
them if it explicitly holds the slug, or if the human approves via
`--ask-human`. Granting `permissions.grant` to an agent makes that
agent a co-administrator — handle with care.

## Bundled skills

Two skills ship with the binary and install to `~/.claude/skills/`
via `tclaude setup --install-agent-skills`:

- **`agent-coord`** — the day-to-day "talk to other agents" skill.
  Triggered by `[system: new agent message #...]` nudges and by user
  prompts asking the agent to coordinate.
- **`agent-rename`** — split out as its own skill so renames are
  obvious in the skill list. Loaded only when the agent decides (or
  is asked) to rename itself.

Re-run `tclaude setup --install-agent-skills` after `go install
…@latest` to refresh the on-disk copies with whatever the new binary
embeds.

## Design notes

- The daemon is **foreground-only**. Run it in a tmux pane / a long-
  running terminal; restart manually after upgrades. (No launchd /
  systemd unit yet.)
- Identity is **peer-cred-based**, not token-based. There's no API
  key to leak.
- `agent_messages` rows accumulate forever for now (no auto-prune);
  bodies are short, so this is fine for a long while.
- The popup is bound to the same daemon process; closing the daemon
  closes the popup listener. The dashboard view planned in
  `docs/plans/agents_todo.md` will reuse the same loopback port.

## Troubleshooting

| Symptom                                                                 | Fix                                                             |
|--------------------------------------------------------------------------|-----------------------------------------------------------------|
| `Error: tclaude agentd is not running.`                                  | Start it: `tclaude agentd serve` (in a non-sandboxed shell).    |
| `Error: not in a shared group with target`                               | Add both convs to the same group with `groups add`.             |
| `Error: selector matches multiple conversations`                         | Use the 8-char conv-id prefix instead of the alias/title.       |
| `Error: caller is not granted permission "<slug>"`                       | Either grant via `permissions grant`, or retry with `--ask-human`. |
| Popup didn't open / opened wrong browser                                 | On WSL, the daemon shells out to `/mnt/c/Windows/System32/cmd.exe /c start`. Check `tclaude agentd serve` logs. |
| `no_tmux` 503 on `agent rename`                                          | Caller has no live tmux session for the daemon to inject into.  |

## See also

- `docs/plans/agent-coord.md` — design doc for `tclaude agent`.
- `docs/plans/agentd.md` — design doc for the daemon (peer-cred
  identity, socket layout).
- `docs/plans/agents_todo.md` — running TODO/DONE list. Read this
  for what's coming next (browser dashboard, system tray icon,
  multicast, group spawn/stop/resume, …).
