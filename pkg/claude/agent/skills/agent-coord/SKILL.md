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

## Renaming yourself

If the human has granted you the `self.rename` permission (in
`~/.tclaude/config.json` under `agent.default_permissions` or
`agent.permission_overrides`), you can change your conversation's
display name:

```bash
tclaude agent rename "code-reviewer-frontend"
```

Behind the scenes the daemon types `/rename <title>` into your own
tmux pane, so any text you'd been typing into the input box is lost.
Don't rename mid-conversation while you have unsubmitted input — wait
for a clean turn.

If you see `Error: caller is not granted permission "self.rename"`,
the human has not opted in. Ask them to add it to
`~/.tclaude/config.json`:

```json
{
  "agent": {
    "default_permissions": ["self.rename"]
  }
}
```

## Etiquette

- **One message, one purpose.** If you have multiple unrelated asks,
  send separate messages with distinct subjects.
- **Use `agent reply <id>`** for replies — the daemon resolves the
  sender from the message ID, so you don't need to copy conv-ids out
  of the headers. The reply inherits `Re: <subject>` automatically.
- **Don't spam.** Tmux nudges interleave with the receiver's input box;
  too many in quick succession will wreck their UX.
- **Don't try to mutate group membership.** The daemon refuses
  `groups create|rm|add|remove` from any caller with a `claude`
  ancestor in its process tree. The human curates the allow-list.

## Troubleshooting

- `Error: tclaude agentd is not running.` → ask the human to start
  the daemon. The CLI no longer falls back to direct DB access.
- `not in a shared group` → ask the human to add you and the peer to
  the same group.
- `selector matches multiple conversations` → use a conv-id prefix
  (the short 8-character form) instead of the alias.

## Installing this skill

The skill is bundled into the `tclaude` binary. Materialise it under
`~/.claude/skills/agent-coord/` with:

```bash
tclaude setup --install-agent-skill
```

That command is idempotent — re-running it overwrites the local copy
with whatever the current binary embeds, so a `go install …@latest`
plus a re-run picks up upstream changes.
