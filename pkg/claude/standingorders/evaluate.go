package standingorders

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Membership is one group the evaluated agent belongs to, with the role it
// holds there. Role is resolved by the caller against the LIVE roster at
// evaluation time — never stored on the order — so a role change takes effect
// without rewriting every order that filters on it. This mirrors how cron
// resolves TargetRole at fire time.
type Membership struct {
	GroupID int64
	Role    string
}

// Event is everything the evaluator needs to know about one lifecycle event.
// It is a plain value with no database or harness handles, so the whole
// decision path is testable without either.
type Event struct {
	// Event is the normalized trigger event, e.g. db.StandingTriggerSessionStart.
	Event string
	// Source is the harness's own source value for the event
	// (startup / resume / clear / compact for SessionStart).
	Source string

	// ConvID is the recipient's current conversation generation. It is the
	// cadence epoch for once-per-generation.
	ConvID string
	// AgentID is the recipient's stable actor key.
	AgentID string
	// Harness is the recipient's harness name, which decides capability.
	Harness string

	Memberships []Membership

	// PayloadTrimmed records that the hook payload reached the evaluator with
	// its tool fields dropped (see db.StandingOutcomeNotEvaluatedTrimmed and
	// session.trimOversizedHookBody). It only changes the answer for triggers
	// that actually read those fields — no SessionStart trigger does — but it
	// is threaded through from the start so the day a tool trigger lands, the
	// "we could not tell" answer already exists and is already distinct from
	// "it did not match".
	PayloadTrimmed bool
}

// Decision is the evaluator's answer for one order against one event.
type Decision struct {
	Order *db.StandingOrder

	// Outcome is the vocabulary shared with the ledger and the dashboard.
	// When Deliver is true it is provisional — the value to record IF the
	// caller's delivery succeeds.
	Outcome string
	// Deliver reports whether the caller should now actually deliver the
	// order's text. The evaluator never delivers anything itself.
	Deliver bool

	Capability Capability
	// Epoch is the cadence key this decision was made under; the caller must
	// record the delivery against it or the cadence check cannot see it.
	Epoch string
	// Detail is a human-readable reason, always populated for a non-delivered
	// outcome. It is what `orders explain` prints.
	Detail string
}

// ShouldRecord reports whether a decision is worth a ledger row.
//
// Out-of-scope and no-match are the overwhelming majority of evaluations and
// carry nothing an operator would come looking for; recording them would bury
// the outcomes that do. Everything else either delivered something or failed
// to, and both are answers someone will eventually need.
func (d Decision) ShouldRecord() bool {
	switch d.Outcome {
	case db.StandingOutcomeOutOfScope, db.StandingOutcomeNoMatch, db.StandingOutcomeDisabled:
		return false
	case db.StandingOutcomeSuppressedCadence:
		// The STEADY STATE of a once-per-generation order, re-reached at every
		// later boundary of the same conversation. Recording it would append a
		// row per boundary forever and leave `ls` reporting the order's last
		// outcome as a suppression when it is working exactly as authored.
		// `explain` still computes it live, which is where it answers a
		// question someone actually asked.
		return false
	}
	return true
}

// triggerReadsToolPayload reports whether a trigger event needs the tool
// fields that the brokered hook path may have dropped. No v1 trigger does;
// this exists so the trimmed-payload branch below is a real branch rather than
// a comment promising future behaviour.
func triggerReadsToolPayload(event string) bool {
	switch event {
	case db.StandingTriggerSessionStart:
		return false
	}
	// An unrecognised trigger is assumed to need the payload. Assuming
	// otherwise would let a future trigger silently evaluate against fields
	// that were never delivered.
	return true
}

// InScope reports whether an order targets the agent described by ev, and why
// not when it does not.
//
// Scope is an AUTHORITY question and is kept strictly separate from trigger
// matching, which is a pattern question. Mixing them would let a matching bug
// turn into a delivery-to-the-wrong-agent bug.
func InScope(o *db.StandingOrder, ev Event) (bool, string) {
	if o.IsGroupTarget() {
		for _, m := range ev.Memberships {
			if m.GroupID != o.GroupID {
				continue
			}
			if o.TargetRole == "" || strings.EqualFold(o.TargetRole, m.Role) {
				return true, ""
			}
			return false, fmt.Sprintf("agent is in group %d but holds role %q, order filters on %q",
				o.GroupID, m.Role, o.TargetRole)
		}
		return false, fmt.Sprintf("agent is not a member of group %d", o.GroupID)
	}

	// Conv-target. Match on the stable agent key when the order has one, so a
	// reincarnation or /clear does not silently drop the order; fall back to
	// the conv id for an order written before its target had an actor.
	if o.TargetAgent != "" {
		if o.TargetAgent == ev.AgentID {
			return true, ""
		}
		return false, "order targets a different agent"
	}
	if o.TargetConv != "" && o.TargetConv == ev.ConvID {
		return true, ""
	}
	return false, "order targets a different conversation"
}

// epochFor returns the cadence key for an order under an event.
func epochFor(o *db.StandingOrder, ev Event) string {
	if o.Cadence == db.StandingCadenceOncePerGeneration {
		return ev.ConvID
	}
	// StandingCadenceAlways has no epoch — every match is its own occasion.
	return ""
}

// DeliveredLookup reports whether an order has already been delivered in a
// cadence epoch. Injected rather than called directly so Evaluate stays a pure
// function over its inputs and the whole decision table is unit-testable.
type DeliveredLookup func(orderID, revision int64, targetConv, epoch string) (bool, error)

// Evaluate decides what happens to one order for one event.
//
// The check ORDER is deliberate and is the part most worth preserving:
//
//  1. disabled     — cheapest, and an operator who disabled an order does not
//     want to see scope or capability complaints about it;
//  2. scope        — an agent must never learn anything about an order that
//     does not target it, including that it exists;
//  3. trigger      — event, then trimmed-payload, then source. Trimmed comes
//     before source because a payload we could not read cannot
//     be reported as "did not match";
//  4. capability   — before cadence, so an order that could never be delivered
//     on this harness never burns its once-per-generation slot;
//  5. cadence      — last, because it is the only check that consumes state.
//
// Step 4 before step 5 matters more than it looks: reversing them would let an
// unsupported harness mark an order delivered and permanently suppress it for
// a conversation that never saw the text.
func Evaluate(o *db.StandingOrder, ev Event, delivered DeliveredLookup) Decision {
	d := Decision{Order: o, Epoch: epochFor(o, ev)}

	if !o.Enabled {
		d.Outcome = db.StandingOutcomeDisabled
		d.Detail = "order is disabled"
		if o.DisabledReason != "" {
			d.Detail += " (" + o.DisabledReason + ")"
		}
		return d
	}

	if ok, why := InScope(o, ev); !ok {
		d.Outcome = db.StandingOutcomeOutOfScope
		d.Detail = why
		return d
	}

	if o.TriggerEvent != ev.Event {
		d.Outcome = db.StandingOutcomeNoMatch
		d.Detail = fmt.Sprintf("order triggers on %s, event was %s", o.TriggerEvent, ev.Event)
		return d
	}

	if ev.PayloadTrimmed && triggerReadsToolPayload(o.TriggerEvent) {
		d.Outcome = db.StandingOutcomeNotEvaluatedTrimmed
		d.Detail = "hook payload was trimmed before evaluation, so this trigger could not be checked; " +
			"this is not the same as the trigger failing to match"
		return d
	}

	if !o.MatchesSource(ev.Source) {
		d.Outcome = db.StandingOutcomeNoMatch
		d.Detail = fmt.Sprintf("event source %q is not in the order's sources (%s)",
			ev.Source, strings.Join(o.TriggerSources, ", "))
		return d
	}

	d.Capability = CapabilityFor(o.Timing, o.TriggerEvent, ev.Harness)
	if !d.Capability.Supported() {
		d.Outcome = db.StandingOutcomeUnsupportedTiming
		d.Detail = d.Capability.Detail
		return d
	}

	if d.Epoch != "" && delivered != nil {
		already, err := delivered(o.ID, o.Revision, ev.ConvID, d.Epoch)
		if err != nil {
			// Fail OPEN, matching the pre-compact guard's house style: a
			// ledger we cannot read is not a reason to withhold guidance the
			// operator asked to be delivered. The worst case is one repeated
			// reminder; the worst case the other way is silence.
			d.Detail = "cadence check failed (" + err.Error() + "); delivering anyway"
		} else if already {
			d.Outcome = db.StandingOutcomeSuppressedCadence
			d.Detail = fmt.Sprintf("already delivered revision %d in this %s epoch",
				o.Revision, o.Cadence)
			return d
		}
	}

	d.Deliver = true
	if d.Capability.Status == StatusDegraded {
		d.Outcome = db.StandingOutcomeDegradedTransport
	} else {
		d.Outcome = db.StandingOutcomeDelivered
	}
	if d.Detail == "" {
		d.Detail = fmt.Sprintf("%s(source=%s) → %s", ev.Event, ev.Source, d.Capability.Transport)
	}
	return d
}

// EvaluateAll runs Evaluate over a set of orders, preserving input order so
// output is stable for both the ledger and `orders explain`.
func EvaluateAll(orders []*db.StandingOrder, ev Event, delivered DeliveredLookup) []Decision {
	out := make([]Decision, 0, len(orders))
	for _, o := range orders {
		out = append(out, Evaluate(o, ev, delivered))
	}
	return out
}

// AuthorLabel renders an order's authorship for display.
//
// "operator" requires the EXPLICIT OperatorAuthored marker. An order with
// neither the marker nor an owner is unattributed, and must say so: both
// columns default to exactly that state, so inferring "operator" from an empty
// owner would let any order that failed to stamp one read to the model as a
// human instruction — on the one channel where authorship is load-bearing.
// This is the inference db.StandingOrder.OperatorAuthored's own comment
// forbids.
func AuthorLabel(o *db.StandingOrder) string {
	switch {
	case o.OperatorAuthored:
		return "operator"
	case o.OwnerAgent != "":
		return "agent " + o.OwnerAgent
	default:
		return "an unattributed source"
	}
}

// RenderContext builds the text injected into an agent's context for the
// orders that are being delivered.
//
// Two properties matter more than formatting:
//
//   - Provenance is in the TEXT, not only in metadata. This is a
//     high-authority channel, and a model reading it should be able to tell
//     that the operator (or a named agent) authored the instruction rather
//     than inferring it from tone.
//   - The order name and revision are included so an agent can ask about a
//     specific order, and so a human reading a transcript can tell which
//     revision was in force at the time.
func RenderContext(decisions []Decision) string {
	var lines []string
	for _, d := range decisions {
		if !d.Deliver {
			continue
		}
		o := d.Order
		author := AuthorLabel(o)
		lines = append(lines, fmt.Sprintf("- [%s@%d, authored by %s] %s",
			o.Name, o.Revision, author, o.Summary))
	}
	if len(lines) == 0 {
		return ""
	}
	return "Standing orders in force for this session:\n" + strings.Join(lines, "\n") +
		"\n(These are durable standing orders, not part of the current request. " +
		"Run `tclaude agent orders ls` to inspect them.)"
}

// NormalizeSource canonicalizes a harness's event source. An empty
// SessionStart source is a cold start, which the harnesses spell "startup"
// when they spell it at all.
//
// It lives here rather than at a call site so every caller shares it — the
// hook path and `orders explain` must not disagree about what `--source ""`
// means, or the CLI would confidently report a non-match for an order the
// real path delivers.
func NormalizeSource(trigger, source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" && trigger == db.StandingTriggerSessionStart {
		return db.StandingSourceStartup
	}
	return source
}
