# Plan: persistent coordination between Claude Code sessions (`tclaude agent`)

Status: draft.

## Goal

Let two (or more) running Claude Code conversations talk to each other without
the human acting as a meat router. Each conversation already has:

- A stable conversation UUID (`session_id` in CC, `conv_id` in tclaude).
- An optional human-readable title (`tclaude conv rename …`, see `conv-rename`
  branch).

We use that title as the agent's name. Sessions can look each other up by name
and send messages that get injected into the *target* conversation's tmux pane
as a system-style nudge ("you have a new message, read it in
`<file>`").

The human controls who can talk to whom by maintaining named *groups*. An
agent can only message agents in a group it shares with the recipient. Group
membership is *not* writable from inside an agent — only the human edits
membership. Agents can only *list* the members of their own groups (so they
know who is reachable).

## Non-goals (for v1)

- Threaded conversations / replies / message IDs surfaced to agents.
- Real-time streaming. Delivery is "drop a file + tmux nudge".
- Cross-machine messaging. Both sender and recipient must run on the host
  whose `~/.tclaude/db.sqlite` we use.
- ACLs finer than "is in a shared group".

## CLI surface

All under a new `tclaude agent` subcommand.

### Looking up other agents

```
tclaude agent whoami
tclaude agent lookup <name>      # name → conv-id (exit 1 if no match, 2 if ambiguous)
tclaude agent ls                 # list agents reachable to me (= union of my groups)
```

`whoami` prints the current conversation's `conv_id` and current title (or
`(unnamed)`).

`lookup` resolves by *current display title* (custom title > summary > first
prompt — same precedence as `conv rename`'s title resolver) and prints the
canonical conv-id on success. Exit codes match `conv rename`: `0/1/2`.

### Sending a message

```
tclaude agent message <target> "body text"
tclaude agent message <target> --stdin
tclaude agent message <target> --file path/to/body.md
```

`<target>` is a conv-id, conv-id prefix, or a current title (resolved like
`lookup`). The sender must:

1. Resolve to its own `conv_id` (via `$TCLAUDE_SESSION_ID` or by walking the
   parent CC pid, same fallback chain `conv rename` uses).
2. Be in at least one group that also contains the target. Otherwise exit
   non-zero with a `not in a shared group` error.

Behaviour:

1. Persist the message in SQLite (`agent_messages`) — body stored inline
   in the `body` column. No separate inbox file.
2. If the target has a live tmux session, inject a nudge via
   `tmux send-keys`:
   ```
   [system: new agent message #<id> from <from-alias> (<from-short>) in group <group>.
   read it with: tclaude agent inbox read <id>; reply with: tclaude agent message <from-short> "..."]
   ```
   followed by Enter — same shape as `task/run.go` already uses for review
   feedback.
3. If the target has no live tmux session, exit `0` anyway and the message
   sits in the DB with `delivered_at` empty. When tclaude next starts a
   session for that conv-id (resume), it can flush undelivered nudges
   (out of scope for v1 but the data model supports it).

Failure modes (with non-zero exits): unknown target, no shared group,
target not in any group, refusing to send to self.

### Group management (human only)

```
tclaude agent groups ls
tclaude agent groups create <group-name> [--descr X]
tclaude agent groups rm <group-name>
tclaude agent groups members <group-name>           # list members
tclaude agent groups add <group-name> <conv> [--alias X] [--role Y] [--descr Z]
tclaude agent groups remove <group-name> <conv>
```

`<conv>` accepts a conv-id, prefix, or current title (same resolver as
`lookup`).

The mutating subcommands (`create`/`rm`/`add`/`remove`) refuse when the
process tree contains a `claude` (or `node`) ancestor — that's the
robust "called from inside an agent" detector. Override precedence:

1. `--allow-from-agent` (per-call, testing escape hatch)
2. `agent.allow_agent_mutate_groups: true` in `~/.tclaude/config.json`
3. Default: refuse

`groups members` and `groups ls` stay open from inside an agent — that's
how the agent finds peers.

### What an agent sees

Inside a conversation, the only commands intended for routine use are:

- `tclaude agent whoami`
- `tclaude agent lookup <name>`
- `tclaude agent ls` — peers (from groups I'm in)
- `tclaude agent groups members <group>` — for groups I'm in
- `tclaude agent message <target> …`

`tclaude agent ls` shows alias, role, description, conv short ID, group(s).

## Data model

New tables, schema version bump from 7 → 8.

```sql
CREATE TABLE agent_groups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    descr       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

CREATE TABLE agent_group_members (
    group_id    INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    conv_id     TEXT NOT NULL,
    alias       TEXT NOT NULL DEFAULT '',
    role        TEXT NOT NULL DEFAULT '',
    descr       TEXT NOT NULL DEFAULT '',
    joined_at   TEXT NOT NULL,
    PRIMARY KEY (group_id, conv_id)
);
CREATE INDEX idx_agent_group_members_conv ON agent_group_members(conv_id);

CREATE TABLE agent_messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id     INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE RESTRICT,
    from_conv    TEXT NOT NULL,
    to_conv      TEXT NOT NULL,
    subject      TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    delivered_at TEXT NOT NULL DEFAULT '',
    read_at      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_agent_messages_to_conv ON agent_messages(to_conv, created_at);
```

`group_id` on `agent_messages` is the group that authorised the send. If the
sender is in multiple shared groups with the target, we pick the first
(deterministic) match and record it.

## Inbox

There is no on-disk inbox. Bodies live inline in
`agent_messages.body`. The receiving agent reads them via
`tclaude agent inbox read <id>` (or `inbox ls` for an overview).

This keeps the picture simple: one source of truth, atomic writes, no
retention plumbing in v1. SQLite handles message-sized text just fine.

Future: a `tclaude agent inbox prune --older-than 30d --read-only` would
delete rows whose `read_at` is set and older than the cutoff. Tracked in
`agents_todo.md`.

## Tmux nudge

Reuses the `clcommon.TmuxCommand("send-keys", ...)` helper already used by
`task/run.go` and the `conv rename` branch:

```go
nudge := fmt.Sprintf(
    "[system: new message from %s in group %s. Read it with: cat %s",
    fromAlias, groupName, inboxPath,
)
clcommon.TmuxCommand("send-keys", "-t", target+":0.0", nudge, "Enter").Run()
```

Caveats inherited from `pushRenameToTmux`:

- Keystrokes interleave with whatever the user is typing in the recipient's
  CC input box. The nudge uses brackets so it stands out, but there's no way
  to fully avoid the race. Acceptable for v1.
- If the tmux session is dead we still record the message in the DB; the next
  resume can flush it.

## Edge cases

| Case | Behaviour |
|---|---|
| Target conv has never had a session (no DB row) | Resolve fails with "unknown target" |
| Target's tmux session is dead | Persist message + write inbox file, exit 0 with warning |
| Sender and target are the same conv | Refuse with "cannot message self" |
| Same name resolves to two convs in different projects | Ambiguous, exit 2; require conv-id prefix |
| Group deleted while message in flight | `ON DELETE RESTRICT` on `agent_messages` prevents the group from being deleted while messages reference it. We require the human to prune messages first |
| Conv removed (jsonl deleted) but still in group | Group membership stays; lookup just falls back to "(unknown)". Pruning out-of-existence convs is a follow-up |

## Implementation outline

1. **DB migration v7→v8** in `pkg/claude/common/db/migrate.go`.
2. **`pkg/claude/common/db/agent.go`** — pure CRUD: groups, members,
   messages.
3. **`pkg/claude/agent/`** package:
   - `agent.go` — top-level `Cmd()` with subcommands
   - `lookup.go` — resolver shared by `lookup`/`message`/`groups` commands.
     Lifted from `conv/rename.go`'s resolver and made non-rename-specific.
   - `whoami.go` — current session detection (same `currentCCSessionID +
     $TCLAUDE_SESSION_ID` chain as rename)
   - `message.go` — auth check, persist, write inbox file, tmux nudge
   - `groups.go` — group create/rm/add/remove/members/ls
   - `inbox.go` — file path conventions + writer
4. **`pkg/claude/claude.go`** — register `agent.Cmd()` in `SubCmds`.
5. **`examples/skills/agent-coord/SKILL.md`** — tells the agent the
   `whoami` / `lookup` / `ls` / `message` flow and what to do when it
   sees the bracketed system nudge.
6. **Tests**:
   - `agent/lookup_test.go` — name & ID resolution incl. ambiguity
   - `agent/groups_test.go` — DB CRUD
   - `agent/message_test.go` — auth check (in-shared-group), inbox file
     writes correct content, tmux call is mockable (extract a
     `nudge func(target, msg) error` seam)
   - `db/agent_test.go` — migration runs cleanly on a v7 DB

## Settled in v1

- **Self-message refusal**: yes, hard refuse.
- **Inbox retention**: keep forever; pruning is on the roadmap (see
  `agents_todo.md`).
- **Cross-project lookup**: global by default. Groups are
  project-agnostic.
- **Multiple shared groups**: pick the first by name; deterministic.
  `--via <group>` is a future option if/when this proves limiting.
- **Storage**: bodies inline in SQLite; no on-disk inbox.
- **Sender-from-agent gate**: walk the parent-process tree for a
  `claude`/`node` ancestor; configurable via
  `agent.allow_agent_mutate_groups`.
