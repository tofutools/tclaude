---
name: agent-coord
description: Coordinate with other Claude Code conversations via `tclaude agent`. Use when you have been put in a group with peer agents and need to look them up, send them messages, or read messages they sent you. Triggered by a `[system: new agent message #...]` line appearing in your conversation, or when the user explicitly asks you to talk to another agent.
---

# Coordinating with other agents

You can talk to other Claude Code conversations on this machine through
`tclaude agent`. Identity comes from the conversation's display name —
the same one `tclaude conv ls` shows. The human controls who can talk to
whom by maintaining named **groups**: you can only message peers who are
in a group with you.

## When to invoke

- A `[system: new agent message #<n> from <alias> in group "<name>" …]`
  line appeared in your conversation. **Read the message first**, then
  decide whether to reply.
- The user asked you to coordinate with another agent (e.g. "ask the
  reviewer agent what they think").

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
[system: new agent message #42 from planner (a1b2c3d4) in group "alpha". read with: tclaude agent inbox read 42. reply with: tclaude agent message a1b2c3d4 "..."]
```

Read it:

```bash
tclaude agent inbox read 42
# alias: tclaude agent mailbox read 42
```

The output has a `Headers:` block (Message-ID, From, To, Group, Subject,
Date) followed by `Body:`. Reading marks the message as read; pass
`--keep-unread` if you want to defer that.

To browse multiple messages:

```bash
tclaude agent inbox ls --unread
```

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

## Etiquette

- **One message, one purpose.** If you have multiple unrelated asks,
  send separate messages with distinct subjects.
- **Quote what you're replying to** in the body. v1 has no thread IDs;
  the receiver only knows it's from you.
- **Don't spam.** Tmux nudges interleave with the receiver's input box;
  too many in quick succession will wreck their UX.
- **Don't try to mutate group membership.** `tclaude agent groups
  create|rm|add|remove` refuses by default when invoked from inside an
  agent. The human curates the allow-list.

## Troubleshooting

- `not in a shared group` → ask the human to add you and the peer to the
  same group.
- `selector matches multiple conversations` → use a conv-id prefix
  (the short 8-character form) instead of the alias.
- `could not detect current conversation` → `$TCLAUDE_SESSION_ID` was
  not propagated and the parent CC pid file is unreadable. Pass an
  explicit conv-id rather than relying on `.`.

## Installing this skill

```bash
ln -s "$(pwd)/examples/skills/agent-coord" ~/.claude/skills/agent-coord
```
