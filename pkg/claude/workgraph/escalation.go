package workgraph

import "time"

// Stuck-node escalation: the pure decision layer for JOH-41 (full stuck/SLA
// detection + escalation). A node that sits with no progress past its effective
// SLA climbs a fixed ladder — warn, then escalate, then a terminal action — so
// the monitor becomes an active assistant rather than a silent watcher. The
// effects (ping the assignee, notify the human, fail the node) and the durable
// at-most-once markers live in agentd's sweep; this file is just the timing +
// classification math, so the ladder is unit-testable without a DB.
//
// This GENERALIZES the JOH-35 liveness sweep (which only ever *failed* a
// no-actor ai node): that flip-to-failed is now the terminal rung of the same
// ladder, reached only after the two softer rungs have had their chance.

// EscalationTier is one rung of the stuck-node ladder. Within a single node
// activation the crossed tier only ever climbs (idle grows while the node's
// updated_at is pinned), so the sweep need only ever act on the single
// highest-crossed rung it has not yet fired.
type EscalationTier int

const (
	TierNone     EscalationTier = iota // not overdue yet
	TierWarn                           // idle >= WarnFraction*T — first soft nudge
	TierEscalate                       // idle >= EscalateFraction*T — raise to the human
	TierTerminal                       // idle >= T — fail (no live actor) or a final urgent notice
)

// String renders the tier as the stable token stored in escalation markers and
// surfaced in events/logs. Changing these strings breaks marker idempotency
// across an upgrade, so treat them as a wire format.
func (t EscalationTier) String() string {
	switch t {
	case TierWarn:
		return "warn"
	case TierEscalate:
		return "escalate"
	case TierTerminal:
		return "terminal"
	default:
		return "none"
	}
}

// Tier thresholds as fractions of the effective SLA T. Hardcoded, NOT config
// (per the JOH-41 lock): a node's single sla knob already moves all three rungs
// together, which is the common need, so exposing the fractions too would only
// bloat the surface. Warn at half the SLA, escalate at four-fifths, terminal at
// the SLA itself.
const (
	WarnFraction     = 0.5
	EscalateFraction = 0.8
)

// EffectiveSLA resolves a node's terminal SLA T (JOH-41), mirroring
// EffectiveMaxVisits' node-over-engine-default shape: the node's own sla field
// if it parses to a positive duration, else the class default — humanDefault for
// a node a human must action (an idle approve-gate is legitimately slower than a
// crashed lint), nonHumanDefault otherwise. A malformed or non-positive sla
// string falls back to the class default rather than failing the node, so a typo
// degrades to a sane threshold instead of disabling the safety net.
func EffectiveSLA(n *Node, isHuman bool, nonHumanDefault, humanDefault time.Duration) time.Duration {
	if n != nil && n.SLA != "" {
		if d, err := time.ParseDuration(n.SLA); err == nil && d > 0 {
			return d
		}
	}
	if isHuman {
		return humanDefault
	}
	return nonHumanDefault
}

// CrossedTier reports the highest rung an idle duration has reached for a node
// whose effective SLA is T. A non-positive T (a node opted out, or a bogus
// default) is treated as "never overdue" so the sweep simply ignores it.
func CrossedTier(idle, T time.Duration) EscalationTier {
	if T <= 0 {
		return TierNone
	}
	switch {
	case idle >= T:
		return TierTerminal
	case idle >= scale(T, EscalateFraction):
		return TierEscalate
	case idle >= scale(T, WarnFraction):
		return TierWarn
	default:
		return TierNone
	}
}

// scale multiplies a duration by a fraction, rounding to the nearest
// nanosecond — kept in one place so the warn/escalate boundaries are computed
// identically in the engine and in tests.
func scale(d time.Duration, frac float64) time.Duration {
	return time.Duration(float64(d) * frac)
}

// TerminalAction is what the terminal rung does for a given node.
type TerminalAction int

const (
	// TermFail flips the node to failed (releasing its parallelism-cap slot) and
	// routes the failure through the normal on_fail / |fail| edge logic. Reserved
	// for the EXACT JOH-35 case — a non-human node with no live actor — because
	// that is the only wedge a fail can safely resolve: a crashed worker, a dead
	// judge, a cap-starved-never-judged node.
	TermFail TerminalAction = iota
	// TermNotify sends one final urgent notice and leaves the node where it is.
	// Used for a node still backed by a LIVE agent (a hung-but-online agent stays
	// an operator cancel, never an auto-reap) and for any human node (auto-failing
	// an idle approve-gate would strand a business process — the human can still
	// act via the dashboard at any time).
	TermNotify
)

// TerminalActionFor picks the terminal rung's action. The auto-fail is
// deliberately narrow: only a non-human node with no live agent. Everything
// else — a live agent's node, any human node — gets a final notice instead, so
// the engine never destroys work a human or a running agent might still finish.
func TerminalActionFor(isHuman, hasLiveAgent bool) TerminalAction {
	if !isHuman && !hasLiveAgent {
		return TermFail
	}
	return TermNotify
}
