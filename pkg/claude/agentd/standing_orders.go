package agentd

import (
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// deliverOpenCodeStandingOrders is the next-turn transport for standing orders
// on OpenCode.
//
// OpenCode has no same-continuation channel: its SSE stream is projected into
// the shared hook state machine by an observation path that calls ApplyHook and
// discards any result, so there is nothing to hand the model inline. What it
// does have is tclaude's own message queue, which is a genuine durable
// transport — the same one cron uses — and which reaches the agent as a queued
// turn.
//
// That difference is exactly what an order's `timing` field declares. An order
// requiring same-continuation is NOT delivered here and is recorded as
// unsupported-timing by the evaluator; only an order that asked for next-turn
// is satisfied by a message. Nothing is silently downgraded.
//
// Failures are soft and logged. This runs on the daemon's event-projection
// goroutine, and a reminder is not worth stalling the projection of a session's
// status.
func deliverOpenCodeStandingOrders(input session.HookCallbackInput, envSessionID string) {
	pending, release := session.ObserveStandingOrders(input, envSessionID)
	defer release()
	for _, p := range pending {
		if p.Body == "" {
			continue
		}
		// The subject is tagged like a cron nudge so a recipient can tell a
		// standing-order reminder from a hand-typed peer message, and so the
		// dashboard groups them recognisably.
		_, err := queueAgentMessage(&db.AgentMessage{
			ToConv:       p.TargetConv,
			ToRecipients: []string{p.TargetConv},
			Subject:      "[standing-order:" + p.Name + "]",
			Body:         p.Body,
			// Authorship comes from the order, never assumed. Stamping every
			// standing-order message operator-authored would present one
			// agent's guidance to another as the human's instruction.
			FromConv:         p.OwnerConv,
			OperatorAuthored: p.OperatorAuthored,
		})
		if err != nil {
			slog.Warn("standing orders: OpenCode message delivery failed",
				"order", p.Name, "target", p.TargetConv, "error", err, "module", "hooks")
		}
		session.RecordStandingMessageDelivery(p, err)
	}
}
