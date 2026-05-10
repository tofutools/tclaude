# Inbox features (2026-05)

Layered onto the v1 inbox / `agent_messages` table.

## `inbox sent` (outbox view)

`tclaude agent inbox sent` lists this conv's outgoing messages
with delivery + read status from the recipient's side. JSON via
`--json`. Backed by `db.ListAgentMessagesFromConv` +
`/v1/inbox?outbox=1`.

## `inbox prune`

`tclaude agent inbox prune --older-than <dur> [--read-only]` —
caller-scoped delete of old `agent_messages` rows. Required
`--older-than` accepts `time.ParseDuration` values plus `Nd` /
`Nw` suffixes. `--read-only` restricts to messages the recipient
has read. Caller-scoped: only deletes rows where `from_conv` or
`to_conv` equals the calling agent's conv-id. Backed by
`db.PruneAgentMessagesForConv` + `/v1/inbox/prune`.

## Threading (schema v10)

`agent_messages.parent_id` column auto-set by `reply`. `inbox
read` shows `In-Reply-To: <id> ("subject")`. `inbox ls` prefixes
reply rows with `↳`. `parent_id` surfaced in `/v1/inbox` rows so
the dashboard can render thread arrows in v2.

## Multi-recipient send `--cc` (schema v18)

`tclaude agent message <primary> --cc <other> --cc <another> "body"`
writes one row per recipient (To + each CC) with the same
email-style audience arrays denormalised onto every row
(`to_recipients` / `cc_recipients` TEXT-as-JSON).

`inbox read` renders `To: ...; CC: ...` from those arrays, so
each receiver sees the full audience without an extra round-trip.
Pre-flight resolve rejects the whole send if any CC selector is
unknown / ambiguous / unreachable, so half-broadcasts can't
silently happen.

## Flush-on-online

Identity middleware kicks a debounced (5s/conv) background flush
whenever it resolves a peer's conv-id. The flush walks
`delivered_at = ''` rows for that recipient, atomically claims
each one, and sends the bracketed nudge. Concurrent flushes are
race-free via `db.ClaimAgentMessageDelivery` (atomic
UPDATE..WHERE delivered_at = '').

## `inbox watch` interactive mailbox v1

Bubbletea TUI under `pkg/claude/agent/inbox_watch.go`. Auto-
refreshes the list every 3s; up/down/k/j to navigate, g/G to
jump to top/bottom, Enter loads + marks read (via the existing
`/v1/messages/{id}` endpoint), esc/q returns to the list / quits.
Background poll suspends while in the read view to avoid list-
shuffle surprises. Reuses the table package + the existing daemon
endpoints — no new server work. Reply / search / target-another-
agent / per-message delete left for v2 (see
`high-prio/inbox-message-ux.md`).

## Multicast

`tclaude agent message group:<name> "..."` fan-out — one row per
member.
