# Human notification channel (Telegram-first, pluggable)

A permission-gated primitive that lets a coordinating agent (the PO)
reach the **human outside the terminal** — on their phone — through an
external transport. Telegram is the first concrete transport; the
abstraction is pluggable so email / Slack can follow without a rewrite.

## Why

The human currently talks to the PO only in the PO's terminal, which
is also full of agent-to-agent traffic and tool output. They want PO
status updates and human-decision questions on a channel they can
check away from the keyboard.

**Scope boundary (confirmed with the human):** tclaude is NOT becoming
an issue tracker and is NOT growing an in-dashboard mailbox / message
store / message UI. The channel integrates an *external* mechanism;
tclaude contributes a thin, uniform "notify-human" primitive with a
pluggable transport — not storage, not a UI.

## Shipped in PR #1 (outbound, PO → human)

- **Config** — `~/.tclaude/config.json` gains a `human_notify` section:
  ```jsonc
  {
    "human_notify": {
      "transport": "telegram",
      "telegram": { "bot_token": "<from @BotFather>", "chat_id": "<target chat>" }
    }
  }
  ```
- **`pkg/claude/humannotify`** — new package. `Transport` interface
  (outbound: `Send`), `InboundTransport` interface (inbound: `Poll` —
  **defined but unimplemented**, the seam for PR #2), `Notification` /
  `InboundReply` types, `Resolve(cfg)` factory, Telegram outbound impl,
  `ResolveTelegramChatIDs` setup helper.
- **`human.notify` permission slug** — `identity.go` const +
  `permissions.go` registry entry. **Not default-granted** (same
  posture as `message.direct` / `agent.delete`): the human grants it to
  the PO. Workers cannot spam the human.
- **`POST /v1/notify-human`** — `pkg/claude/agentd/notify_human.go`.
  Gated on `human.notify` (humans bypass; `X-Tclaude-Ask-Human` popup
  escape hatch honored). Resolves the configured transport, sends.
- **`tclaude agent notify-human "<body>"`** CLI verb —
  `--file`/`-`, `--subject`, `--ask-human`. Plus a `resolve-chat-id`
  subcommand that calls Telegram `getUpdates` once and prints the
  chats the bot has seen, so the human can obtain their `chat_id`.
- **`human-notify` bundled skill** — tells the PO the verb exists,
  when to use it, the permission requirement, and etiquette.
- **Tests** — `humannotify/telegram_test.go` (unit, real Telegram
  transport vs `httptest.Server`) and
  `agentd/notify_human_flow_test.go` (flow: permission gating,
  delivery, not-configured). Injectable transport seam in agentd
  (`SetHumanNotifyTransportForTest`); no real API calls in CI.

## Open for PR #2 (inbound, human → PO)

The human replies in Telegram and the reply reaches the PO.

- Implement `InboundTransport` on the Telegram transport (`Poll` over
  `getUpdates` long-poll; the cursor packs the `offset`).
- `startHumanNotifyPoller(cronStop)` in `runServe` — a background
  goroutine alongside `startCronScheduler`, started only when the
  resolved transport implements `InboundTransport`.
- Route a reply to the PO via a direct tmux nudge
  (`[system: human replied via Telegram: "..."]`, reusing
  `injectTextAndSubmit`) — no synthetic `FromConv`, decoupled from the
  `agent_messages` model.
- **Reply routing** — most-recent-notifier for v1; optional
  `telegram_message_id → conv-id` correlation table (routing metadata,
  no message content — confirmed inside the scope boundary) for
  precise routing when the human uses Telegram's native reply.
- **Offline-PO handling** — decided direction: option (c), bounded
  retry (hold the Telegram offset for N polls) then fall back. Settle
  the exact bound when scoping PR #2.

## Relevant source files

- `pkg/claude/common/config/config.go` — `HumanNotifyConfig`.
- `pkg/claude/humannotify/` — transport abstraction + Telegram impl.
- `pkg/claude/agentd/notify_human.go` — handler + transport seam.
- `pkg/claude/agentd/serve.go` — `buildMux` route registration; PR #2
  adds the poller to `runServe`.
- `pkg/claude/agentd/identity.go`, `permissions.go` — the slug.
- `pkg/claude/agent/notify_human.go` — CLI verb.
- `pkg/claude/agent/skills/human-notify/SKILL.md`, `skills.go`.

## Telegram Bot API notes

- Outbound: `POST https://api.telegram.org/bot<token>/sendMessage`
  (`chat_id`, `text`).
- Inbound (PR #2): `getUpdates` long-poll (`offset`, `timeout`,
  `allowed_updates`). No webhook — the daemon is behind NAT; long-poll
  needs only outbound connectivity, and Telegram holds undelivered
  updates server-side until `offset` advances.
- Human supplies a `bot_token` (`@BotFather` → `/newbot`) and a
  `chat_id` (`resolve-chat-id` helper, after messaging the bot once).
