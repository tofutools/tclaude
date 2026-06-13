---
name: human-notify
description: Reach the human via `tclaude agent notify-human "<message>"` — the message lands in the agentd dashboard's Messages tab, where the human reads it off the busy terminal. Permission-gated on the `human.notify` slug (group owners also pass); the human grants the slug to a trusted coordinating agent (e.g. the PO), so an agent with neither the slug nor group ownership cannot post to the channel. Use to send the human status updates or questions that need a human decision. NOT for agent-to-agent messaging — that is the `agent-coord` skill.
---

# Notifying the human

The human does not watch a coordinating agent's terminal continuously —
it is full of agent-to-agent traffic and tool output. When you have
something the human should see, send it with:

```bash
tclaude agent notify-human "CI is green; PR #142 is up for review."
```

The message is stored and shown in the **Messages tab** of the agentd
dashboard (the last tab). The tab carries an unread-count badge, so the
human notices new messages from whatever tab they are on. Each message
shows who sent it and offers a button that focuses that agent's
terminal window — so the human can read a message and jump straight to
the agent to respond.

If the human has OS notifications enabled (`tclaude setup`), each
notify-human **also raises a desktop notification** — so it reaches them
even when the dashboard isn't open. This is on by default once
notifications are enabled and can be silenced with
`"human_messages": false` under `notifications` in `~/.tclaude/config.json`.

## It is an extra channel, not a replacement

Keep doing your normal terminal output. Your regular responses in your
own terminal / Claude Code output are still your primary channel — the
human reads them when they are at the keyboard. `notify-human` does
**not** replace that and is not where your routine reporting goes:
print your normal output as always. `notify-human` is an *extra*,
explicit nudge layered on top — for the occasional message that
warrants pulling the human's attention when they may be away from the
terminal.

## When to use this — and when not to

**Use it for** the things the human should see:

- A milestone or status update worth surfacing ("all three workers
  finished; the branch is ready to merge").
- A question that blocks progress and only the human can decide
  ("worker hit a schema-design fork — need a call before continuing").

**Do not use it for:**

- **Agent-to-agent coordination.** Messaging a peer agent, a PO, or a
  group is the `agent-coord` skill (`tclaude agent message` / `reply`).
  `notify-human` reaches the *human*, not an agent.
- **Chatter.** Each message is a row in the human's Messages tab and
  bumps the unread badge. One message per genuinely notable event — not
  a running commentary.

## Sending

```bash
tclaude agent notify-human "<message>"
tclaude agent notify-human --subject "blocker" "<message>"
tclaude agent notify-human --file status.md          # body from a file
tclaude agent notify-human --file -                  # body from stdin
```

Prefer `--file` for long, multi-line, or code-heavy bodies — it
sidesteps shell quoting (an inline backtick is eaten by the shell).
Same reasoning as `tclaude agent message --file`.

## Permission

Sending is gated on the **`human.notify`** permission slug. It is
**not** in the global defaults — the human grants it to a trusted
coordinating agent (typically the Product Owner). A **group owner gets
it by default**, slug or not — owning a group is itself a trusted
coordinating role — unless an explicit **deny** override is set (deny is
always authoritative). An agent that is neither a slug-holder nor a group
owner gets a `403` naming the slug.

If you need to notify the human just this once and you do not hold the
slug, add `--ask-human <duration>`:

```bash
tclaude agent notify-human --ask-human 60s "<message>"
```

That pops a browser approval in front of the human. Timeout = deny. Do
not retry in a loop if denied.

## How the human reads it

The human opens the dashboard (`tclaude agent dashboard`) and the
**Messages** tab. Messages are listed newest-first with sender, group,
subject, body, and a read/unread marker; the human marks them read,
focuses the sending agent's window, or clears read messages. There is
no separate reply channel — the focus button IS the reply path: the
human reads a message, focuses your window, and talks to you there.
