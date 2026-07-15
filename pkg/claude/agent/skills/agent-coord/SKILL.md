---
name: agent-coord
description: >-
  Coordinate with other tclaude-managed Claude Code or Codex CLI conversations
  via `tclaude agent`. Routes through a `tclaude agentd` daemon (the human
  starts it; you don't). Use when you've been put in a group with peer agents
  and need to look them up, send them messages, or read messages they sent you.
  Triggered by a `[system: new agent message #...]` line appearing in your
  conversation, or when the user explicitly asks you to talk to another agent.
---

# Coordinating with other agents

You can talk to other tclaude-managed agent conversations on this machine through
`tclaude agent`. Every command goes through a daemon (`tclaude agentd
serve`) which the human starts once per machine. Your identity is
resolved from the connecting socket peer's PID — no tokens to manage,
and resumed or forked sessions keep working because the daemon re-reads
your current conv-id on every call.

The human controls who can talk to whom by maintaining named **groups**.
Messaging a peer you share a group with always works. Messaging an agent
*outside* your group — including an ungrouped solo agent — additionally
requires the `message.direct` permission; without it the send is refused.

## When to invoke

- A `[system: new agent message #<n> for you. fetch with: tclaude
  agent inbox read <n>]` line appeared in your conversation. **Read the
  message first**, then decide whether to reply.
- The user asked you to coordinate with another agent (e.g. "ask the
  reviewer agent what they think").

## Prerequisite: daemon must be running

If you see `Error: tclaude agentd is not running.`, ask the human to
start it (`tclaude agentd serve` in a non-sandboxed terminal). Without
it the CLI deliberately refuses to fall back to direct DB access — that
keeps the auth model honest.

## Discovering peers

```bash
tclaude agent whoami      # who am I (stable agent_id and display name)?
tclaude agent ls          # peers reachable via shared groups
```

`ls` shows name, role, description, the peer's **agent_id** (short form),
and which groups you share with each peer. The `agent_id` (`agt_…`) is the
**stable, canonical handle** — unlike a conv-id it never changes when the
agent reincarnates or clones, so it's the right thing to copy when you want
to address a peer.

## Reading a message you were nudged about

The system nudge looks like:

```
[system: new agent message #42 for you. fetch with: tclaude agent inbox read 42]
```

Fetch it:

```bash
tclaude agent inbox read 42
# alias: tclaude agent mailbox read 42
```

The output has a `Headers:` block (Message-ID, From, To, Group, Subject,
Date, **Reply-To**, **Reply-Cmd**) followed by `Body:`. Reading marks
the message as read; pass `--keep-unread` if you want to defer that.

The `Reply-To` and `Reply-Cmd` headers are how you reply — see below.

To browse multiple messages:

```bash
tclaude agent inbox ls --unread
```

## Replying

The fastest path is `tclaude agent reply <id>`:

```bash
tclaude agent reply 42 "Got it, will look at the diff this afternoon."
tclaude agent reply 42 --file reply.md          # body from a file
tclaude agent reply 42 --stdin <<EOF
multi-line reply
body
EOF
```

`reply` looks up message 42, sends the body to its sender, and inherits
the original subject as `Re: <subject>` (override with `--subject`).

For a long, multi-line, or code-heavy reply, prefer `--file` — see
"Long or code-heavy bodies" below.

If you'd rather address the sender directly (e.g. starting a brand-new
thread), use the `Reply-To` value from the headers as the target of
`tclaude agent message`.

## Sending a message

```bash
tclaude agent message <peer> "your message text"
tclaude agent message <peer> --file plan.md     # body from a file
tclaude agent message <peer> --subject "ack" --stdin <<EOF
multi-line
body
EOF
```

`<peer>` is the peer's **stable `agent_id`** (full or a unique `agt_…`
prefix — the preferred, rotation-immune handle), its display name, or a
conv-id / short conv prefix (still accepted, but it changes when the peer
reincarnates). A peer in one of your groups always works. Messaging an
agent outside your group needs
the `message.direct` permission — without it the send fails loudly,
naming the slug; ask the human to grant it (or get a time-bounded grant
via `tclaude agent sudo`).

If the target has a live tmux session, they get a system nudge on their
next turn. If they're offline, the message stays queued in their inbox
and they'll see it on resume.

## Long or code-heavy bodies — use `--file`

`tclaude agent message` and `tclaude agent reply` both accept
`--file <path>` to read the body from a file instead of typing it on the
command line (`--file -` reads stdin, so you can pipe a body in). The
file content is sent verbatim — newlines and indentation preserved.

**Reach for `--file` whenever the body is long, multi-line, or contains
code.** Passing such a body inline is fragile:

- **Backticks get eaten by the shell.** A body typed on the command line
  is processed by the shell first, and an unescaped backtick starts a
  command substitution — so ``message peer "see `foo`"`` runs `foo` and
  drops the result into your message. `$(…)` has the same problem.
  Quotes, `$`, and newlines all need careful escaping too.
- **A body read from `--file` is immune.** Nothing re-interprets it —
  backticks, `$(…)`, code blocks, and ``` ```fences``` ``` all survive
  exactly as written in the file.

So: write the body to a file (or a heredoc piped via `--file -`) and let
tclaude read it. Don't fight shell quoting — for any non-trivial body,
`--file` is the clean answer. The same flag exists on the lifecycle
verbs (`reincarnate`, `clone`) and on `cron add` for the same reason.

## Broadcasting to a group

Prefix the target with `group:` to fan out to every member of a group
except yourself:

```bash
tclaude agent message group:reviewer-team "Heads up: PR #42 ready"
tclaude agent message group:reviewer-team --subject "review" --file plan.md
```

The group can be addressed three ways:

- **By name** — `group:reviewer-team`.
- **By numeric id** — `group:7` (a fallback for when no group is *named*
  `7`; a matching name always wins over the id).
- **Your own group** — a bare `group:` with nothing after the colon
  resolves to the single group you belong to. It is an error if you are
  in zero or more than one group; name the group explicitly then.

```bash
tclaude agent message group: "status update for my team"
```

### Role-filtered broadcast

Add `--role <role>` to reach only the members holding that role (matched
case-insensitively). `--role` is only valid with a `group:` target —
it is an error on a 1:1 message:

```bash
tclaude agent message group:team-A --role reviewer "please review PR #42"
tclaude agent message group: --role PO "blocker — need a decision"
```

You must be a member or owner of the group to broadcast. The CLI prints
the resolved recipient count plus a per-recipient summary (delivered vs
queued); a `--role` that matches nobody is a visible `0 recipients`
no-op, not an error. Replies come back as normal direct messages —
there's no automatic "reply-all".

**Use sparingly.** Each member's tmux pane gets the same nudge, so
broadcasting is the load-bearing equivalent of a chat-wide mention.
Prefer direct messages for one-to-one conversations.

## Renaming yourself

`tclaude agent rename "<title>"` lets you change your conversation's
display name. The mechanics, permission requirement (`self.rename`),
and config-file edits live in the dedicated **`agent-rename`** skill —
load that one when you need to rename, not this one.

## Managing your own context window

Long-running agents can act on context pressure via
`tclaude agent context-info`, `tclaude agent compact`, and
`tclaude agent reincarnate`. The default depends on the harness:
reincarnation is primarily for Claude Code, whose compaction is comparatively
slow and lossy. Codex CLI has effective, efficient automatic compaction, so let
a Codex agent run to full context and auto-compact. Do not reincarnate a Codex
agent merely to free context space. An explicit human request or another
deliberate replacement reason still takes precedence. Project policy decides
when a Claude Code agent should act. The mechanics, `self.compact` permission
(self-reincarnation needs no slug), disk-handoff convention, and
follow-up etiquette live in the dedicated **`agent-lifecycle`** skill —
load that one when you need to compact or reincarnate yourself, not this
one.

## Spawning workers — default resolution

When you delegate work by spawning a fresh agent
(`tclaude agent spawn <group> …`, needs `groups.spawn`), the launch shape is
**not** simply "the flags you passed, else the harness default". Each launch
field (`--harness`, `--model`, `--effort`, `--sandbox`, `--ask-for-approval`,
`--ask-user-question-timeout`) is resolved independently through this
precedence, highest first:

1. the **explicit flag**
2. **`--profile`** — a saved spawn profile you name on the command line
3. the **group's default spawn profile**
4. the **global (dashboard) default spawn profile**
5. the **harness's own default**

The harness is resolved through that full chain first; the remaining fields
are then checked against it. An incompatible explicit flag is a loud error with
guidance to pass a matching `--harness` or field value. An incompatible value
from a lower profile tier is ignored and falls through, but the resolved-shape
echo discloses the skip. Other launch flags never infer or pin the vendor.

> ⚠️ **A default profile carries its own harness, so an unset `--harness` can
> silently flip vendor.** `spawn` without `--harness`/`--model` does **not**
> mean "Claude Code". If a default profile at tier 3 or 4 selects `codex`, a
> no-flag spawn lands on **codex** with that profile's model — even though the
> per-flag help calls Claude Code the harness default. This is exactly how a
> lead who expected Claude Code got workers running on a Codex GPT model.
>
> **Policy-bound spawns — where a specific model or vendor is required — MUST
> pass explicit `--harness` + `--model`, or a `--profile` that pins them.**
> Omitting them inherits whatever default profile is set, including a different
> vendor.

Inspect the defaults before you spawn:

```bash
tclaude agent profiles default show   # the global default spawn profile (if any)
tclaude agent groups ls               # PROFILE column = each group's default profile
```

Profiles may expose semantic aliases. `tclaude agent profiles ls` shows them,
and `agent spawn --profile` accepts either the primary name or an alias. Prefer
an intent-revealing alias such as `codex-reviewer` when team guidance names one;
the resolved launch echo discloses both the canonical profile and alias used.

The spawn output now **echoes the resolved launch shape and where each value
came from**, so you can catch a surprise at a glance:

```
Spawned agt_… in group "team"
  Label:   team-worker
  Harness: codex (global default profile "gpt5.6-sol-high")
  Model:   gpt-5.6-sol (global default profile "gpt5.6-sol-high")
  Effort:  high (global default profile "gpt5.6-sol-high")
```

The provenance tag reads `explicit`, `profile "<name>"`,
`group default profile "<name>"`, `global default profile "<name>"`, or
`harness default`. An ignored ambient value is appended explicitly, for example
`profile "codex-kit" model ignored (not valid for claude)`. If a field shows a
profile tier you didn't intend, re-spawn with the explicit flag.

## Etiquette

- **One message, one purpose.** If you have multiple unrelated asks,
  send separate messages with distinct subjects.
- **Use `agent reply <id>`** for replies — the daemon resolves the
  sender from the message ID, so you don't need to copy conv-ids out
  of the headers. The reply inherits `Re: <subject>` automatically.
- **Don't spam.** Tmux nudges interleave with the receiver's input box;
  too many in quick succession will wreck their UX.
- **Don't mutate group membership unless granted.** Mutating
  subcommands (`groups create|rm|add|remove|update-member`) are
  permission-gated. By default agents can't run them. Humans bypass
  the gate. Slugs: `groups.create`, `groups.rm`, `groups.stop`,
  `groups.resume`, `groups.retire`, `member.add`, `member.remove`,
  `member.redesignate`. A **group owner gets the lifecycle verbs for
  its own group by default** — `groups.spawn` / `groups.stop` /
  `groups.retire` / `groups.resume`, plus `human.notify` — without an
  explicit grant (an explicit deny override still suppresses them).

  Permissions live in three durable places:
  - **Defaults** — `agent.default_permissions` in
    `~/.tclaude/config.json`. Granted to every agent.
  - **Group grants** — live additive grants for every current member of an
    active group, configured in the dashboard's Groups tab under the group ⚙
    menu. They are membership policy rather than spawn-time copies; an
    individual agent's explicit deny still wins.
  - **Per-agent grants** — SQLite (`agent_permissions`), additive on
    top of defaults. Managed via the CLI:

    ```bash
    tclaude agent permissions slugs                       # what slugs exist
    tclaude agent permissions ls                          # everything
    tclaude agent permissions ls <conv-or-title>          # effective for one agent
    tclaude agent permissions grant default <slug>        # add to defaults
    tclaude agent permissions grant <conv> <slug>         # add per-agent
    tclaude agent permissions revoke <conv> <slug>
    ```

    `grant`/`revoke` are themselves gated (slugs `permissions.grant` /
    `permissions.revoke`) so by default only the human can run them.

  **Ad-hoc human approval.** If you need an action just this once,
  every mutating command takes `--ask-human <duration>`:

  ```bash
  tclaude agent groups create foo --ask-human 30s
  ```

  This creates an access request in the dashboard Messages tab with
  Approve / Deny buttons. The CLI blocks until the human decides or the
  timeout fires. **Timeout = Deny** so an unattended request never
  silently grants. Cap is 300s. If denied or timed out, accept the
  answer; don't retry in a loop.

## Troubleshooting

- `Error: tclaude agentd is not running.` → ask the human to start
  the daemon. The CLI no longer falls back to direct DB access.
- `not in a shared group` → ask the human to add you and the peer to
  the same group.
- `selector matches multiple conversations` → address the peer by its
  stable `agent_id` (shown by `tclaude agent ls` / `whoami`), which is
  unambiguous; a conv-id prefix also works but rotates on reincarnation.

## Installing the agent skills

The agent skills (this one, `agent-rename`, …) are bundled into the
`tclaude` binary. Materialise them under the supported user skill roots
(`~/.claude/skills/<name>/` for Claude Code and
`~/.agents/skills/<name>/` plus `$CODEX_HOME/skills/<name>/` for
Codex CLI) with:

```bash
tclaude setup --install-agent-skills
```

That command is idempotent — re-running it overwrites the local
copies with whatever the current binary embeds, so a
`go install …@latest` plus a re-run picks up upstream changes.
