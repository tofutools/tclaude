---
name: agent-coord
description: Coordinate with other Claude Code conversations via `tclaude agent`. Routes through a `tclaude agentd` daemon (the human starts it; you don't). Use when you've been put in a group with peer agents and need to look them up, send them messages, or read messages they sent you. Triggered by a `[system: new agent message #...]` line appearing in your conversation, or when the user explicitly asks you to talk to another agent.
---

# Coordinating with other agents

You can talk to other Claude Code conversations on this machine through
`tclaude agent`. Every command goes through a daemon (`tclaude agentd
serve`) which the human starts once per machine. Your identity is
resolved from the connecting socket peer's PID — no tokens to manage,
and `/fork` keeps working because the daemon re-reads your current
conv-id on every call.

The human controls who can talk to whom by maintaining named **groups**;
you can only message peers who are in a group with you.

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
tclaude agent whoami      # who am I (conv-id and display name)?
tclaude agent ls          # peers reachable via shared groups
```

`ls` shows alias, role, description, conv short ID, and which groups you
share with each peer.

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
tclaude agent reply 42 --stdin <<EOF
multi-line reply
body
EOF
```

`reply` looks up message 42, sends the body to its sender, and inherits
the original subject as `Re: <subject>` (override with `--subject`).

If you'd rather address the sender directly (e.g. starting a brand-new
thread), use the `Reply-To` value from the headers as the target of
`tclaude agent message`.

## Sending a message

```bash
tclaude agent message <peer> "your message text"
tclaude agent message <peer> --subject "ack" --stdin <<EOF
multi-line
body
EOF
tclaude agent message <peer> --file plan.md
```

`<peer>` is the peer's display name, conv-id, or short ID. The send fails
loudly if you do not share a group with the target — that's intentional;
only the human controls allow-listing.

If the target has a live tmux session, they get a system nudge on their
next turn. If they're offline, the message stays queued in their inbox
and they'll see it on resume.

## Broadcasting to a whole group

Prefix the target with `group:` to fan out to every member of that
group except yourself:

```bash
tclaude agent message group:reviewer-team "Heads up: PR #42 ready"
tclaude agent message group:reviewer-team --subject "review" --file plan.md
```

You must be a member of the group to broadcast. The CLI prints a
per-recipient summary (delivered vs queued) so you can see who got
the nudge. Replies come back as normal direct messages — there's
no automatic "reply-all".

**Use sparingly.** Each member's tmux pane gets the same nudge, so
broadcasting is the load-bearing equivalent of a chat-wide mention.
Prefer direct messages for one-to-one conversations.

## Renaming yourself

`tclaude agent rename "<title>"` lets you change your conversation's
display name. The mechanics, permission requirement (`self.rename`),
and config-file edits live in the dedicated **`agent-rename`** skill —
load that one when you need to rename, not this one.

## Managing your own context window

Long-running agents can self-throttle on context pressure via
`tclaude agent context-info`, `tclaude agent compact`, and
`tclaude agent reincarnate`. Compact at ~50% on a 1M context window
or ~75% on a 200k window to avoid context rot. Reincarnate is the
heavier path that swaps you out for a fresh successor with the same
identity. The mechanics, permission slugs (`self.compact` /
`self.reincarnate`), disk-handoff convention, and follow-up etiquette
live in the dedicated **`agent-lifecycle`** skill — load that one
when you need to compact or reincarnate yourself, not this one.

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
  `groups.resume`, `member.add`, `member.remove`, `member.redesignate`.

  Permissions live in two places:
  - **Defaults** — `agent.default_permissions` in
    `~/.tclaude/config.json`. Granted to every agent.
  - **Per-agent grants** — SQLite (`agent_permissions`), additive on
    top of defaults. Managed via the CLI:

    ```bash
    tclaude agent permissions slugs                       # what slugs exist
    tclaude agent permissions ls                          # everything
    tclaude agent permissions ls <conv-or-alias>          # effective for one agent
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

  This pops a browser window in front of the human with Approve / Deny
  buttons. The CLI blocks until they click or the timeout fires.
  **Timeout = Deny** so an unattended popup never silently grants. Cap
  is 300s. If denied or timed out, accept the answer; don't retry in a
  loop.

## Troubleshooting

- `Error: tclaude agentd is not running.` → ask the human to start
  the daemon. The CLI no longer falls back to direct DB access.
- `not in a shared group` → ask the human to add you and the peer to
  the same group.
- `selector matches multiple conversations` → use a conv-id prefix
  (the short 8-character form) instead of the alias.

## Installing the agent skills

The agent skills (this one, `agent-rename`, …) are bundled into the
`tclaude` binary. Materialise them under `~/.claude/skills/<name>/`
with:

```bash
tclaude setup --install-agent-skills
```

That command is idempotent — re-running it overwrites the local
copies with whatever the current binary embeds, so a
`go install …@latest` plus a re-run picks up upstream changes.
