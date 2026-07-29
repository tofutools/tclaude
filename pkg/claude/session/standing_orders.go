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

	// Recording is DEFERRED until the response has actually been written.
	//
	// The two orderings fail differently and the asymmetry decides it. Record
	// first and a failed write (a closed pipe, a harness that went away)
	// leaves a once-per-generation order marked delivered for a conversation
	// that never saw the text — permanently silent, with a ledger that says
	// otherwise, and no way to notice. Record after and a failed record means
	// the reminder is repeated at the next boundary. One is unrecoverable and
	// invisible; the other is mild and self-correcting.
	commit := func() {
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
	}

	text := standingorders.RenderContext(decisions)
	if text == "" {
		// Nothing is being written, so there is no write to wait for; the
		// non-delivery outcomes still belong in the ledger.
		commit()
		return HookResponse{}
	}
	slog.Info("standing orders: delivering context",
		"conv_id", ev.ConvID, "event", input.HookEventName, "source", input.Source,
		"orders", deliveredNames(decisions), "module", "hooks")
	return HookResponse{AdditionalContext: text, commit: commit}
}

// ObserveStandingOrders evaluates standing orders for an event that arrived on
// an OBSERVATION-ONLY path — one with no response channel back to the model.
// OpenCode is the case that exists: its SSE projector calls ApplyHook and
// discards any result, so nothing this function decides can be handed to the
// model inline.
//
// It returns the orders that should be delivered out-of-band on the message
// transport, and records every other outcome itself. The caller (agentd, which
// owns the message queue) performs the actual sends and calls
// RecordStandingMessageDelivery for each.
//
// Splitting it this way is what keeps OpenCode from silently getting nothing.
// Before this existed, a same-continuation order aimed at an OpenCode agent
// produced no delivery AND no ledger row, which is the failure this feature is
// supposed to prevent: the operator would see an order that looked healthy and
// had never reached anyone.
func ObserveStandingOrders(input HookCallbackInput, envSessionID string) []PendingStandingMessage {
	trigger := standingOrderTriggerFor(input.HookEventName)
	if trigger == "" || input.AgentID != "" {
		return nil
	}
	ev, ok := standingOrderEvent(input, envSessionID, trigger)
	if !ok {
		return nil
	}
	orders, err := db.ListEnabledStandingOrdersForEvent(trigger)
	if err != nil || len(orders) == 0 {
		return nil
	}

	var pending []PendingStandingMessage
	for _, d := range standingorders.EvaluateAll(orders, ev, db.StandingOrderDeliveredInEpoch) {
		if d.Deliver && d.Capability.Transport == db.StandingTransportMessage {
			// The one case this path can actually satisfy. Recording is
			// deferred to the caller, which knows whether the send succeeded.
			pending = append(pending, PendingStandingMessage{
				OrderID:       d.Order.ID,
				OrderRevision: d.Order.Revision,
				Name:          d.Order.Name,
				Body:          standingorders.RenderContext([]standingorders.Decision{d}),
				TargetConv:    ev.ConvID,
				Epoch:         d.Epoch,
				Harness:       ev.Harness,
				Detail:        d.Detail,
				// Authorship is carried from the ORDER, never assumed. An
				// agent-authored order delivered as an operator-authored
				// message would present one agent's guidance to another as
				// the human's instruction — the same misattribution that
				// keeps activation off the unauthenticated CLI.
				OperatorAuthored: d.Order.OperatorAuthored,
				OwnerConv:        d.Order.OwnerConv,
			})
			continue
		}
		outcome, detail := d.Outcome, d.Detail
		if d.Deliver {
			// It matched and the harness could carry it, but the transport its
			// timing selects has no implementation on this path. Not the
			// harness's limitation — tclaude's — and the operator deciding
			// whether to re-author the order needs to tell those apart.
			outcome = db.StandingOutcomeTransportUnimplemented
			detail = "matched, but the " + d.Capability.Transport +
				" transport is not wired on this harness's observation path"
		}
		if !(standingorders.Decision{Outcome: outcome}).ShouldRecord() {
			continue
		}
		if _, err := db.RecordStandingDelivery(&db.StandingDelivery{
			OrderID: d.Order.ID, OrderRevision: d.Order.Revision,
			TargetConv: ev.ConvID, Epoch: d.Epoch, Outcome: outcome,
			Transport: d.Capability.Transport, Harness: ev.Harness, Detail: detail,
		}); err != nil {
			slog.Warn("standing orders: failed to record observed outcome",
				"error", err, "order", d.Order.Name, "module", "hooks")
		}
	}
	return pending
}

// PendingStandingMessage is one standing order that matched on an
// observation-only path and must be delivered as a durable message by the
// caller. It carries everything the ledger row needs so the caller does not
// have to re-derive any of it.
type PendingStandingMessage struct {
	OrderID       int64
	OrderRevision int64
	Name          string
	Body          string
	TargetConv    string
	Epoch         string
	Harness       string
	Detail        string
	// OperatorAuthored and OwnerConv carry the ORDER's real authorship onto
	// the message, so the recipient sees who actually wrote the guidance.
	OperatorAuthored bool
	OwnerConv        string
}

// RecordStandingMessageDelivery closes the ledger for a message-transport
// delivery the caller attempted. sendErr nil means it went out.
//
// A failed send records a problem outcome rather than nothing, and
// deliberately does NOT record a delivery — so the cadence check still lets
// the next boundary retry rather than treating a failure as satisfied.
func RecordStandingMessageDelivery(p PendingStandingMessage, sendErr error) {
	outcome, detail := db.StandingOutcomeDelivered, p.Detail
	if sendErr != nil {
		outcome = db.StandingOutcomeDeliveryFailed
		detail = "message send failed: " + sendErr.Error()
	}
	if _, err := db.RecordStandingDelivery(&db.StandingDelivery{
		OrderID: p.OrderID, OrderRevision: p.OrderRevision,
		TargetConv: p.TargetConv, Epoch: p.Epoch, Outcome: outcome,
		Transport: db.StandingTransportMessage, Harness: p.Harness, Detail: detail,
	}); err != nil {
		slog.Warn("standing orders: failed to record message delivery",
			"error", err, "order", p.Name, "module", "hooks")
	}
}

// standingOrderEvent assembles the evaluator's input from the session row and
// the group roster. It reports ok=false when the recipient cannot be
// identified, because an order must never be evaluated — let alone delivered —
// against an agent tclaude cannot place.
func standingOrderEvent(input HookCallbackInput, envSessionID, trigger string) (standingorders.Event, bool) {
	// The conversation is taken from the RESOLVED SESSION ROW, never from the
	// payload, and the payload's claim must agree with it.
	//
	// This is a scope boundary, not a lookup convenience. applyHook returns nil
	// both when it applied an event and when its foreign-process guard DROPPED
	// one, so an event naming somebody else's conv still reaches this function.
	// Trusting input.ConvID would let any caller that can post a hook — the
	// `session hook-callback` subcommand reads a payload from stdin and writes
	// the answer to stdout — read back the standing orders targeting an agent
	// it merely named, and, worse, have the resulting `delivered` ledger row
	// consume that conversation's once-per-generation slot so the real agent
	// is silenced with a ledger that claims success.
	//
	// Requiring agreement means a caller can at most ask about the session it
	// was already resolved as. On the brokered path that row comes from the
	// daemon's own pid records, so this is a real authentication boundary. On
	// the direct path envSessionID is TCLAUDE_SESSION_ID, which is
	// caller-controlled — the check still closes the cheap "name any conv"
	// hole, but an unsandboxed agent that forges BOTH values is bounded only
	// by the pre-existing trust model for direct database writes.
	state, err := loadStandingOrderSession(envSessionID)
	if err != nil || state == nil {
		return standingorders.Event{}, false
	}
	convID := strings.TrimSpace(state.ConvID)
	if convID == "" || convID != strings.TrimSpace(input.ConvID) {
		return standingorders.Event{}, false
	}

	ev := standingorders.Event{
		Event:   trigger,
		Source:  strings.ToLower(strings.TrimSpace(input.Source)),
		ConvID:  convID,
		Harness: db.DefaultHarness,
	}
	if h := strings.TrimSpace(state.Harness); h != "" {
		ev.Harness = h
	}
	// An empty SessionStart source means a cold start; the harnesses spell it
	// "startup" when they spell it at all. Normalization lives in the
	// evaluator so every caller — including `orders explain` — shares it.
	ev.Source = standingorders.NormalizeSource(trigger, ev.Source)

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

// loadStandingOrderSession resolves the session row a hook event belongs to.
// A missing envSessionID is a refusal rather than a fallback: without a
// resolved row there is no authority for which conversation the event is
// about, and standing orders are cross-agent data.
func loadStandingOrderSession(envSessionID string) (*SessionState, error) {
	if strings.TrimSpace(envSessionID) == "" {
		return nil, nil
	}
	return LoadSessionState(envSessionID)
}
