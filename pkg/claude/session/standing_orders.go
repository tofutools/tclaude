package session

import (
	"context"
	"log/slog"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/standingorders"
)

// standingOrderTriggerFor maps a harness hook event onto a standing-order
// trigger event, or "" when no trigger corresponds to it.
//
// This is the whole of the hot path's filter, and it is deliberately a switch
// over a string rather than a database lookup: hooks run inside the agent's
// turn on every event, so the overwhelmingly common answer ("this event has no
// triggers") must cost nothing. Only an event that maps to a real trigger goes
// on to open the database.
func standingOrderTriggerFor(hookEventName string) string {
	switch hookEventName {
	case "SessionStart":
		return db.StandingTriggerSessionStart
	}
	return ""
}

// standingOrderResponse evaluates the standing orders that match one hook
// event and returns the context to place in front of the model.
//
// It is called after applyHook has recorded the event, and it never returns an
// error: this runs in the agent's critical path, behind a 20s client deadline
// and a per-session lock, and no reminder is worth disrupting a turn over. A
// failure here degrades to "no reminder this time", is logged, and self-heals
// on the next boundary. That is the same fail-open posture the pre-compact
// guard takes for the same reason.
//
// Only same-continuation deliveries happen here. An order whose timing is
// next-turn and whose recipient has no hook channel is delivered by agentd on
// the message path instead; this function reports that case honestly through
// the ledger rather than pretending the hook covered it.
func standingOrderResponse(ctx context.Context, input HookCallbackInput, envSessionID string) HookResponse {
	trigger := standingOrderTriggerFor(input.HookEventName)
	if trigger == "" {
		return HookResponse{}
	}

	// A SessionStart carrying agent_id came from inside an in-harness
	// subagent, which shares the main conversation's conv-id. Subagent
	// inheritance is deliberately not in v1 (an order would otherwise be
	// re-injected into every short-lived Explore agent that will never act on
	// it), so those events are not evaluated at all.
	if input.AgentID != "" {
		return HookResponse{}
	}

	ev, ok := standingOrderEvent(input, envSessionID, trigger)
	if !ok {
		return HookResponse{}
	}

	orders, err := db.ListEnabledStandingOrdersForEvent(trigger)
	if err != nil {
		slog.Warn("standing orders: failed to read orders, skipping this event",
			"error", err, "event", input.HookEventName, "module", "hooks")
		return HookResponse{}
	}
	if len(orders) == 0 {
		return HookResponse{}
	}

	decisions := standingorders.EvaluateAll(orders, ev, db.StandingOrderDeliveredInEpoch)

	// Record before returning. The text reaches the model as a side effect of
	// this function returning, so a ledger row written afterwards could be
	// lost while the reminder still landed — and a cadence that cannot see a
	// delivery would repeat it on every boundary.
	for _, d := range decisions {
		if !d.ShouldRecord() {
			continue
		}
		if _, err := db.RecordStandingDelivery(&db.StandingDelivery{
			OrderID:       d.Order.ID,
			OrderRevision: d.Order.Revision,
			TargetConv:    ev.ConvID,
			Epoch:         d.Epoch,
			Outcome:       d.Outcome,
			Transport:     d.Capability.Transport,
			Harness:       ev.Harness,
			Detail:        d.Detail,
		}); err != nil {
			slog.Warn("standing orders: failed to record delivery",
				"error", err, "order", d.Order.Name, "module", "hooks")
		}
	}

	text := standingorders.RenderContext(decisions)
	if text == "" {
		return HookResponse{}
	}
	slog.Info("standing orders: delivering context",
		"conv_id", ev.ConvID, "event", input.HookEventName, "source", input.Source,
		"orders", deliveredNames(decisions), "module", "hooks")
	return HookResponse{AdditionalContext: text}
}

// standingOrderEvent assembles the evaluator's input from the session row and
// the group roster. It reports ok=false when the recipient cannot be
// identified, because an order must never be evaluated — let alone delivered —
// against an agent tclaude cannot place.
func standingOrderEvent(input HookCallbackInput, envSessionID, trigger string) (standingorders.Event, bool) {
	convID := strings.TrimSpace(input.ConvID)
	if convID == "" {
		return standingorders.Event{}, false
	}

	ev := standingorders.Event{
		Event:   trigger,
		Source:  strings.ToLower(strings.TrimSpace(input.Source)),
		ConvID:  convID,
		Harness: db.DefaultHarness,
	}
	// An empty SessionStart source means a cold start; the harnesses spell it
	// "startup" when they spell it at all, and normalizing here keeps the
	// operator's source filter meaning one thing.
	if ev.Source == "" && trigger == db.StandingTriggerSessionStart {
		ev.Source = db.StandingSourceStartup
	}

	if envSessionID != "" {
		if state, err := LoadSessionState(envSessionID); err == nil && state != nil {
			if h := strings.TrimSpace(state.Harness); h != "" {
				ev.Harness = h
			}
		}
	}

	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		slog.Warn("standing orders: failed to resolve agent for conv",
			"error", err, "conv_id", convID, "module", "hooks")
		return standingorders.Event{}, false
	}
	ev.AgentID = agentID

	// A conversation with no actor cannot be in a group, so it is only ever
	// reachable by a conv-targeted order — leave memberships empty rather than
	// failing the whole evaluation.
	if agentID != "" {
		groups, err := db.ListGroupsForAgent(agentID)
		if err != nil {
			slog.Warn("standing orders: failed to read group memberships",
				"error", err, "agent", agentID, "module", "hooks")
			return standingorders.Event{}, false
		}
		for _, g := range groups {
			ev.Memberships = append(ev.Memberships, standingorders.Membership{
				GroupID: g.ID,
				Role:    roleInGroup(g.ID, convID),
			})
		}
	}
	return ev, true
}

// roleInGroup resolves the role a conversation holds in a group, against the
// LIVE roster rather than anything stored on the order. A role change must
// take effect without rewriting every order that filters on it — the same
// property cron's TargetRole has.
func roleInGroup(groupID int64, convID string) string {
	members, err := db.ListAgentGroupMembers(groupID)
	if err != nil {
		return ""
	}
	for _, m := range members {
		if m.ConvID == convID {
			return m.Role
		}
	}
	return ""
}

func deliveredNames(decisions []standingorders.Decision) string {
	var names []string
	for _, d := range decisions {
		if d.Deliver {
			names = append(names, d.Order.Name)
		}
	}
	return strings.Join(names, ",")
}
