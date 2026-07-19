package processexec

import "time"

// ContactPauseReasonHumanPreemption is the shared pause reason for automation
// suspended because a human interacted with the live performer session. Both
// the legacy host and the schema-7 executor pause and release with this exact
// reason.
const ContactPauseReasonHumanPreemption = "human interaction with live agent session"

// ContactHumanPreemptionGrace is how long observed human activity must stand
// before automation pauses.
const ContactHumanPreemptionGrace = 5 * time.Second

// ContactSnapshot is the normalized schema-neutral view of one live contact
// schedule. The legacy v6 host and the schema-7 executor both project their
// durable state into this shape so nudge/escalation policy exists exactly
// once.
type ContactSnapshot struct {
	Cadence           time.Duration
	Budget            int
	Used              int
	Paused            bool
	PauseReason       string
	NextContactAt     time.Time
	LastContactedAt   time.Time
	LastRecoveredAt   time.Time
	EscalatedAt       time.Time
	HumanInteractedAt time.Time
}

// ActivitySince is the instant adapter Activity queries measure from: the
// later of the last outbound nudge and the last observed recovery.
func (s ContactSnapshot) ActivitySince() time.Time {
	since := s.LastContactedAt
	if s.LastRecoveredAt.After(since) {
		since = s.LastRecoveredAt
	}
	return since
}

type ContactSend string

const (
	ContactSendNone     ContactSend = ""
	ContactSendNudge    ContactSend = "nudge"
	ContactSendEscalate ContactSend = "escalate"
)

// ContactDecision is the complete tick decision. Callers apply the fields in
// declaration order — reset, latch clear, latch, pause — and only then act on
// Send, which was evaluated against the post-flag schedule state.
type ContactDecision struct {
	// Reset applies performer-recovery semantics: budget and escalation
	// cleared, latches released, cadence restarted. ResetAt is the observed
	// recovery instant.
	Reset   bool
	ResetAt time.Time
	// ClearLatch removes the human-interaction latch (and a pause held for
	// it) because delivery metadata proved the activity was tclaude's own
	// automation.
	ClearLatch bool
	// LatchAt, when non-zero, records observed human interaction.
	LatchAt time.Time
	// Pause suspends automation with ContactPauseReasonHumanPreemption.
	Pause bool
	Send  ContactSend
}

// DecideContact is the single pure nudge/escalation policy shared by the v6
// host and the schema-7 executor. It never performs I/O; the caller owns
// durable application and external sends. now is the tick instant.
func DecideContact(s ContactSnapshot, activity Activity, now time.Time) ContactDecision {
	var d ContactDecision
	if activity.Recovered && activity.At.After(s.LastRecoveredAt) {
		d.Reset = true
		d.ResetAt = activity.At.UTC()
		s.Used = 0
		s.EscalatedAt = time.Time{}
		s.LastRecoveredAt = d.ResetAt
		s.HumanInteractedAt = time.Time{}
		if s.Paused && s.PauseReason == ContactPauseReasonHumanPreemption {
			s.Paused, s.PauseReason = false, ""
		}
		if s.Cadence > 0 {
			s.NextContactAt = now.Add(s.Cadence)
		}
	}
	// A delivery-correlated UserPromptSubmit is our own automation, not human
	// preemption. It may arrive after an earlier tick tentatively latched the
	// same hook as human activity, so clear that latch once delivery metadata
	// makes the origin unambiguous.
	if activity.AutomatedDelivery &&
		(!s.HumanInteractedAt.IsZero() || s.PauseReason == ContactPauseReasonHumanPreemption) &&
		!activity.At.Before(s.HumanInteractedAt) {
		d.ClearLatch = true
		s.HumanInteractedAt = time.Time{}
		if s.Paused && s.PauseReason == ContactPauseReasonHumanPreemption {
			s.Paused, s.PauseReason = false, ""
		}
	}
	if activity.HumanInteracted && activity.At.After(s.HumanInteractedAt) {
		d.LatchAt = activity.At.UTC()
		s.HumanInteractedAt = d.LatchAt
	}
	if !s.HumanInteractedAt.IsZero() && now.Sub(s.HumanInteractedAt) >= ContactHumanPreemptionGrace && !s.Paused {
		d.Pause = true
		s.Paused, s.PauseReason = true, ContactPauseReasonHumanPreemption
	}
	if s.Paused || s.NextContactAt.IsZero() || now.Before(s.NextContactAt) {
		return d
	}
	if s.Used < s.Budget {
		d.Send = ContactSendNudge
	} else if s.EscalatedAt.IsZero() {
		d.Send = ContactSendEscalate
	}
	return d
}
