package standingorders

import (
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The types below are the read-only wire shape the dashboard renders
// (TCL-843). They live here rather than in the dashboard package so there is
// one authority for the payload: the UI half of the prototype consumes
// OrderView without re-deriving trigger labels, capability, or outcome
// spellings from the raw row.

// TargetView is an order's scope, in the same vocabulary as a cron job's.
type TargetView struct {
	Kind      string `json:"kind"`
	Agent     string `json:"agent,omitempty"`
	Conv      string `json:"conv,omitempty"`
	GroupID   int64  `json:"group_id"`
	GroupName string `json:"group_name,omitempty"`
	Role      string `json:"role,omitempty"`
}

// TriggerView carries a pre-rendered label so every surface prints the same
// string instead of composing its own from event + sources.
type TriggerView struct {
	Event   string   `json:"event"`
	Sources []string `json:"sources,omitempty"`
	Label   string   `json:"label"`
}

// EvaluationView is the most recent ledger row for an order.
type EvaluationView struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	// Problem is OutcomeIsProblem(Outcome), carried on the payload because the
	// dashboard runs in a browser and cannot call the Go helper. Without it
	// the UI has to keep its own outcome-to-severity branch, which is exactly
	// the vocabulary drift this shared payload exists to prevent: a new
	// outcome added here would silently render as "fine" over there.
	Problem    bool   `json:"problem"`
	Transport  string `json:"transport,omitempty"`
	Harness    string `json:"harness,omitempty"`
	Detail     string `json:"detail,omitempty"`
	TargetConv string `json:"target_conv,omitempty"`
}

// OrderView is one standing order as the dashboard sees it.
type OrderView struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`

	Enabled          bool   `json:"enabled"`
	DisabledReason   string `json:"disabled_reason,omitempty"`
	OperatorAuthored bool   `json:"operator_authored"`

	OwnerAgent string `json:"owner_agent,omitempty"`
	OwnerConv  string `json:"owner_conv,omitempty"`

	Target TargetView `json:"target"`

	Summary string      `json:"summary"`
	Trigger TriggerView `json:"trigger"`

	Timing  string `json:"timing"`
	Cadence string `json:"cadence"`

	// Capability is the worst case across the harnesses this order can
	// ACTUALLY reach, resolved by the caller from the conv target or the live
	// group roster. It is null when the caller could not resolve them, because
	// "we don't know who this reaches" is a different answer from "it reaches
	// nobody" and the UI should not render one as the other.
	Capability *Capability `json:"capability"`
	// PlatformCapability is the worst case across every harness tclaude
	// supports. It answers "could this order be authored to work everywhere",
	// not "does it reach its targets" — see ReduceCapability.
	PlatformCapability  Capability            `json:"platform_capability"`
	CapabilityByHarness map[string]Capability `json:"capability_by_harness"`

	LastEvaluation *EvaluationView `json:"last_evaluation"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewOrderView projects a stored order into the wire shape.
//
// groupName is resolved by the caller (which has the group registry) and may
// be empty; latest may be nil when the order has never produced a ledger row.
//
// targetHarnesses are the harnesses the order can actually reach — the
// recipient's harness for a conv target, or the distinct harnesses on the live
// roster for a group target. Pass nil when they cannot be resolved; Capability
// is then null rather than a guess.
func NewOrderView(o *db.StandingOrder, groupName string, latest *db.StandingDelivery, targetHarnesses []string) OrderView {
	v := OrderView{
		ID:               o.ID,
		Name:             o.Name,
		Revision:         o.Revision,
		Enabled:          o.Enabled,
		DisabledReason:   o.DisabledReason,
		OperatorAuthored: o.OperatorAuthored,
		OwnerAgent:       o.OwnerAgent,
		OwnerConv:        o.OwnerConv,
		Target: TargetView{
			Kind:      o.TargetKind,
			Agent:     o.TargetAgent,
			Conv:      o.TargetConv,
			GroupID:   o.GroupID,
			GroupName: groupName,
			Role:      o.TargetRole,
		},
		Summary: o.Summary,
		Trigger: TriggerView{
			Event:   o.TriggerEvent,
			Sources: o.TriggerSources,
			Label:   o.TriggerLabel(),
		},
		Timing:              o.Timing,
		Cadence:             o.Cadence,
		PlatformCapability:  PlatformCapability(o.Timing, o.TriggerEvent),
		CapabilityByHarness: CapabilityByHarness(o.Timing, o.TriggerEvent),
		CreatedAt:           o.CreatedAt,
		UpdatedAt:           o.UpdatedAt,
	}
	if len(targetHarnesses) > 0 {
		reachable := ReduceCapability(o.Timing, o.TriggerEvent, targetHarnesses)
		v.Capability = &reachable
	}
	if latest != nil {
		v.LastEvaluation = &EvaluationView{
			At:         latest.CreatedAt,
			Outcome:    latest.Outcome,
			Problem:    OutcomeIsProblem(latest.Outcome),
			Transport:  latest.Transport,
			Harness:    latest.Harness,
			Detail:     latest.Detail,
			TargetConv: latest.TargetConv,
		}
	}
	return v
}

// OutcomeIsProblem reports whether an outcome should render as a warning
// state. Everything other than a clean delivery or a clean non-match means the
// operator asked for something they did not fully get, and the UI should not
// have to keep its own copy of that list.
func OutcomeIsProblem(outcome string) bool {
	switch outcome {
	case db.StandingOutcomeDelivered, db.StandingOutcomeNoMatch,
		// Suppressed-by-cadence is a once-per-generation order behaving as
		// authored, not a fault. Flagging it would mark a correctly working
		// order as a problem for the rest of the conversation.
		db.StandingOutcomeSuppressedCadence:
		return false
	}
	return true
}
