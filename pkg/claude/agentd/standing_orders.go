package agentd

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/standingorders"
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
		err := queueStandingOrderMessage(p, nil)
		if err != nil {
			slog.Warn("standing orders: OpenCode message delivery failed",
				"order", p.Name, "target", p.TargetConv, "error", err, "module", "hooks")
		}
		session.RecordStandingMessageDelivery(p, err)
	}
}

func standingOrderAgentMessage(p session.PendingStandingMessage) *db.AgentMessage {
	return &db.AgentMessage{
		ToConv: p.TargetConv, ToRecipients: []string{p.TargetConv},
		Subject: "[standing-order:" + p.Name + "]", Body: p.Body,
		FromConv: p.OwnerConv, OperatorAuthored: p.OperatorAuthored,
	}
}

func queueStandingOrderMessage(
	p session.PendingStandingMessage,
	debounce *db.StandingDebounce,
) error {
	if p.Body == "" {
		return nil
	}
	message := standingOrderAgentMessage(p)
	var err error
	if debounce == nil {
		_, err = db.InsertStandingOrderAgentMessage(
			message, p.OrderID, p.OrderRevision)
	} else {
		_, err = db.ConsumeStandingDebounceIntoAgentMessage(
			debounce, message, &db.StandingDelivery{
				OrderID: p.OrderID, OrderRevision: p.OrderRevision,
				TargetConv: p.TargetConv, TargetAgent: p.TargetAgent,
				Epoch: p.Epoch, Outcome: db.StandingOutcomeDelivered,
				Transport: db.StandingTransportMessage,
				Harness:   p.Harness, Detail: p.Detail,
			})
	}
	if err == nil {
		enqueueDeliveryForConv(p.TargetConv)
	}
	return err
}

const standingOrderDebounceTick = time.Second

func startStandingOrderDebounceScheduler(stop <-chan struct{}) {
	go func() {
		runStandingOrderDebounceTick(time.Now())
		ticker := time.NewTicker(standingOrderDebounceTick)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				runStandingOrderDebounceTick(now)
			}
		}
	}()
}

func runStandingOrderDebounceTick(now time.Time) {
	due, err := db.ListDueStandingDebounces(now)
	if err != nil {
		slog.Warn("standing orders: list due debounce failed", "error", err)
		return
	}
	for _, candidate := range due {
		fireStandingOrderDebounce(candidate.OrderID, candidate.TargetAgent, now)
	}
}

func fireStandingOrderDebounce(orderID int64, targetAgent string, now time.Time) {
	release, acquired := session.LockStandingOrderAgentDelivery(
		context.Background(), orderID, targetAgent)
	if !acquired {
		return
	}
	defer release()

	pending, err := db.GetDueStandingDebounce(orderID, targetAgent, now)
	if err != nil || pending == nil {
		return
	}
	order, err := db.GetStandingOrder(orderID)
	if err != nil {
		return
	}
	if order == nil || !order.Enabled ||
		order.Revision != pending.OrderRevision ||
		order.DebounceSeconds <= 0 ||
		order.Timing != db.StandingTimingNextTurn {
		_ = db.DeleteStandingDebounce(orderID, targetAgent)
		return
	}
	if order.OwnerAgent != "" {
		owner, ownerErr := db.GetAgent(order.OwnerAgent)
		if ownerErr != nil {
			return
		}
		if !owner.Active() {
			_ = db.DeleteStandingDebounce(orderID, targetAgent)
			return
		}
	}
	recipient, err := db.GetAgent(targetAgent)
	if err != nil {
		return
	}
	if !recipient.Active() {
		_ = db.DeleteStandingDebounce(orderID, targetAgent)
		return
	}
	currentConv := recipient.CurrentConvID
	inScope, scopeErr := standingDebounceInScope(order, targetAgent)
	if scopeErr != nil {
		return
	}
	if currentConv == "" || !inScope {
		_ = db.DeleteStandingDebounce(orderID, targetAgent)
		return
	}
	harnessName := pending.Harness
	state, stateErr := session.FindSessionByConvID(currentConv)
	if stateErr != nil {
		return
	}
	if state != nil && strings.TrimSpace(state.Harness) != "" {
		harnessName = state.Harness
	}
	capability := standingorders.CapabilityForOrder(order, harnessName)
	if !capability.Supported() || capability.Transport != db.StandingTransportMessage {
		_ = db.DeleteStandingDebounce(orderID, targetAgent)
		recordStandingDebounceOutcome(order, pending, currentConv, harnessName,
			db.StandingOutcomeUnsupportedTiming, capability, capability.Detail)
		return
	}
	epoch := ""
	if order.Cadence == db.StandingCadenceOncePerGeneration {
		epoch = currentConv
		already, checkErr := db.StandingOrderDeliveredInEpoch(
			order.ID, order.Revision, currentConv, epoch)
		if checkErr != nil {
			return
		}
		if already {
			_ = db.DeleteStandingDebounce(orderID, targetAgent)
			recordStandingDebounceOutcome(order, pending, currentConv, harnessName,
				db.StandingOutcomeSuppressedCadence, capability,
				"debounced candidate was already delivered in this conversation generation")
			return
		}
	}
	if order.CooldownSeconds > 0 {
		last, checkErr := db.LatestSuccessfulStandingDeliveryAt(
			order.ID, order.Revision, targetAgent)
		if checkErr != nil {
			return
		}
		if !last.IsZero() &&
			now.Before(last.Add(time.Duration(order.CooldownSeconds)*time.Second)) {
			_ = db.DeleteStandingDebounce(orderID, targetAgent)
			recordStandingDebounceOutcome(order, pending, currentConv, harnessName,
				db.StandingOutcomeSuppressedCooldown, capability,
				"debounced candidate reached its quiet edge during cooldown")
			return
		}
	}
	decision := standingorders.Decision{
		Order: order, Deliver: true, Outcome: db.StandingOutcomeDelivered,
		Capability: capability, Epoch: epoch,
		Detail: "trailing-edge debounce window completed",
	}
	message := session.PendingStandingMessage{
		OrderID: order.ID, OrderRevision: order.Revision, Name: order.Name,
		Body:       standingorders.RenderContext([]standingorders.Decision{decision}),
		TargetConv: currentConv, TargetAgent: targetAgent, Epoch: epoch,
		Harness: harnessName, Detail: decision.Detail,
		OperatorAuthored: order.OperatorAuthored, OwnerConv: order.OwnerConv,
	}
	err = queueStandingOrderMessage(message, pending)
	if err != nil {
		slog.Warn("standing orders: debounced message delivery failed",
			"order", order.Name, "agent", targetAgent, "error", err)
		return
	}
	// The successful ledger row committed atomically with the durable message
	// and pending-edge delete. Recording it afterward would leave a crash gap
	// in which cadence/cooldown could schedule a duplicate.
}

func standingDebounceInScope(
	order *db.StandingOrder,
	targetAgent string,
) (bool, error) {
	if order.IsGlobalTarget() {
		return true, nil
	}
	if !order.IsGroupTarget() && order.TargetAgent == targetAgent {
		return true, nil
	}
	if order.IsGroupTarget() {
		member, err := db.FindAgentMemberInGroup(order.GroupID, targetAgent)
		if err != nil {
			return false, err
		}
		if member != nil &&
			(order.TargetRole == "" ||
				strings.EqualFold(order.TargetRole, member.Role)) {
			return true, nil
		}
	}
	for _, groupID := range order.AdditionalGroupIDs {
		member, err := db.FindAgentMemberInGroup(groupID, targetAgent)
		if err != nil {
			return false, err
		}
		if member != nil {
			return true, nil
		}
	}
	return false, nil
}

func recordStandingDebounceOutcome(
	order *db.StandingOrder,
	pending *db.StandingDebounce,
	convID, harnessName, outcome string,
	capability standingorders.Capability,
	detail string,
) {
	epoch := pending.Epoch
	if order.Cadence == db.StandingCadenceOncePerGeneration {
		epoch = convID
	}
	_, _ = db.RecordStandingDelivery(&db.StandingDelivery{
		OrderID: order.ID, OrderRevision: order.Revision,
		TargetConv: convID, TargetAgent: pending.TargetAgent,
		Epoch: epoch, Outcome: outcome,
		Transport: capability.Transport, Harness: harnessName, Detail: detail,
	})
}

func RunStandingOrderDebounceTickForTest(now time.Time) {
	runStandingOrderDebounceTick(now)
}
