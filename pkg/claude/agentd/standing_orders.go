package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	// Pending covers prompt_async acceptance uncertainty, including a daemon
	// crash after the server accepted the prompt but before the response was
	// observed. Active survives long tool-heavy turns and projector restarts;
	// the normal Stop boundary clears it much earlier.
	standingOrderOriginPendingTTL = 5 * time.Minute
	standingOrderOriginActiveTTL  = 24 * time.Hour
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
		message := &db.AgentMessage{
			ToConv:       p.TargetConv,
			ToRecipients: []string{p.TargetConv},
			Subject:      "[standing-order:" + p.Name + "]",
			Body:         p.Body,
			// Authorship comes from the order, never assumed. Stamping every
			// standing-order message operator-authored would present one
			// agent's guidance to another as the human's instruction.
			FromConv:         p.OwnerConv,
			OperatorAuthored: p.OperatorAuthored,
		}
		_, err := db.InsertStandingOrderAgentMessage(
			message, p.OrderID, p.OrderRevision)
		if err == nil {
			// The trusted origin row is durable before the async nudge worker
			// can submit the prompt. It re-arms the turn handshake on every
			// retry, so an expired failed attempt can never deliver without
			// origin suppression.
			enqueueDeliveryForConv(p.TargetConv)
		}
		if err != nil {
			slog.Warn("standing orders: OpenCode message delivery failed",
				"order", p.Name, "target", p.TargetConv, "error", err, "module", "hooks")
		}
		session.RecordStandingMessageDelivery(p, err)
	}
}
