---
name: human-notify
description: Reach the human OUTSIDE the terminal — on their phone — via `tclaude agent notify-human "<message>"`. Routes through the agentd daemon to a configured external transport (Telegram today). Permission-gated on the `human.notify` slug, which the human grants to a trusted coordinating agent (e.g. the PO) so workers cannot spam the channel. Use to send the human status updates or questions that need a human decision when they are away from the terminal. NOT for agent-to-agent messaging — that is the `agent-coord` skill.
---

# Notifying the human outside the terminal

The human does not watch their terminal continuously. When you have
something the human should see while they are away from the keyboard —
a status update worth knowing, or a question only they can answer —
send it to their external channel (Telegram) with:

```bash
tclaude agent notify-human "CI is green; PR #142 is up for review."
```

It goes through the `tclaude agentd` daemon to whatever transport the
human configured. The human sees it on their phone.

## When to use this — and when not to

**Use it for** the few things the human genuinely needs while away:

- A milestone or status update worth an interruption ("all three
  workers finished; branch is ready to merge").
- A question that blocks progress and only the human can decide
  ("worker hit a schema-design fork — need a call before continuing").

**Do not use it for:**

- **Agent-to-agent coordination.** Messaging a peer agent, a PO, or a
  group is the `agent-coord` skill (`tclaude agent message` /
  `reply`). `notify-human` reaches the *human*, not an agent.
- **Chatter.** Every notification lands on the human's phone. One
  message per genuinely notable event — not a running commentary.
  Over-messaging trains the human to ignore the channel.

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

The human's notification shows who sent it (your display title) and
which group you are in, so they have context without asking.

## Permission

Sending is gated on the **`human.notify`** permission slug. It is
**not** granted to every agent by default — the human grants it to a
single trusted coordinating agent (typically the Product Owner) so the
channel cannot be spammed. If you are not that agent you will get a
`403` naming the slug.

If you need to notify the human just this once and you do not hold the
slug, add `--ask-human <duration>`:

```bash
tclaude agent notify-human --ask-human 60s "<message>"
```

That pops a browser approval in front of the human (if they are at the
terminal — which, note, somewhat defeats the purpose). Timeout = deny.
Do not retry in a loop if denied.

## If it is not configured

`Error: ... no human-notify transport configured` means the human has
not set up the channel. It is the human's setup task, not yours — tell
the human (in the terminal) that the channel is not configured rather
than retrying.

## Replies

Today the channel is **one-way** (you → human). The human reads your
notification on their phone but answers back in the terminal as usual.
A future update will route the human's Telegram reply back to you; the
CLI surface will not change when it does.

## Setup (human-run, for reference)

The human configures the channel once, in `~/.tclaude/config.json`:

```jsonc
{
  "human_notify": {
    "transport": "telegram",
    "telegram": { "bot_token": "<from @BotFather>", "chat_id": "<target chat>" }
  }
}
```

They obtain the `bot_token` from `@BotFather` (`/newbot`), then — after
sending the new bot a message — resolve the `chat_id` with:

```bash
tclaude agent notify-human resolve-chat-id
```

That lists the chats the bot has seen so they can pick the right id.
