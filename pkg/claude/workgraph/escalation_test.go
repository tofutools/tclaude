package workgraph

import (
	"testing"
	"time"
)

func TestCrossedTier(t *testing.T) {
	const T = 100 * time.Minute // warn at 50m, escalate at 80m, terminal at 100m
	cases := []struct {
		idle time.Duration
		want EscalationTier
	}{
		{0, TierNone},
		{49 * time.Minute, TierNone},
		{50 * time.Minute, TierWarn},     // exactly WarnFraction*T
		{79 * time.Minute, TierWarn},
		{80 * time.Minute, TierEscalate}, // exactly EscalateFraction*T
		{99 * time.Minute, TierEscalate},
		{100 * time.Minute, TierTerminal}, // exactly T
		{500 * time.Minute, TierTerminal}, // far past — stays terminal (idle only climbs)
	}
	for _, c := range cases {
		if got := CrossedTier(c.idle, T); got != c.want {
			t.Errorf("CrossedTier(%v, %v) = %v, want %v", c.idle, T, got, c.want)
		}
	}
}

func TestCrossedTier_NonPositiveSLA(t *testing.T) {
	// A node that opted out (or a bogus default) is never overdue.
	for _, T := range []time.Duration{0, -5 * time.Minute} {
		if got := CrossedTier(time.Hour, T); got != TierNone {
			t.Errorf("CrossedTier(1h, %v) = %v, want TierNone", T, got)
		}
	}
}

func TestCrossedTier_Monotonic(t *testing.T) {
	// The sweep relies on the crossed tier never decreasing as idle grows within
	// one activation (updated_at pinned) — that is what lets it fire only the
	// single highest-crossed rung and never backfill a lower one.
	const T = 30 * time.Minute
	prev := TierNone
	for idle := time.Duration(0); idle <= 2*T; idle += time.Minute {
		got := CrossedTier(idle, T)
		if got < prev {
			t.Fatalf("tier decreased at idle=%v: %v < %v", idle, got, prev)
		}
		prev = got
	}
}

func TestEffectiveSLA(t *testing.T) {
	const (
		nonHuman = 15 * time.Minute
		human    = 60 * time.Minute
	)
	cases := []struct {
		name    string
		node    *Node
		isHuman bool
		want    time.Duration
	}{
		{"nil node non-human → non-human default", nil, false, nonHuman},
		{"nil node human → human default", nil, true, human},
		{"empty sla non-human → non-human default", &Node{}, false, nonHuman},
		{"empty sla human → human default", &Node{}, true, human},
		{"valid per-node sla overrides (non-human)", &Node{SLA: "3m"}, false, 3 * time.Minute},
		{"valid per-node sla overrides (human)", &Node{SLA: "2h"}, true, 2 * time.Hour},
		{"malformed sla falls back to class default", &Node{SLA: "notaduration"}, false, nonHuman},
		{"zero sla falls back to class default", &Node{SLA: "0s"}, true, human},
		{"negative sla falls back to class default", &Node{SLA: "-5m"}, false, nonHuman},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffectiveSLA(c.node, c.isHuman, nonHuman, human); got != c.want {
				t.Errorf("EffectiveSLA = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTerminalActionFor(t *testing.T) {
	cases := []struct {
		isHuman, hasLiveAgent bool
		want                  TerminalAction
	}{
		{false, false, TermFail},   // non-human + no live actor = the only auto-fail (JOH-35 case)
		{false, true, TermNotify},  // live agent — never auto-reap a working agent
		{true, false, TermNotify},  // human node — never auto-fail an approve-gate
		{true, true, TermNotify},   // human node with an agent somehow attached — still never fail
	}
	for _, c := range cases {
		if got := TerminalActionFor(c.isHuman, c.hasLiveAgent); got != c.want {
			t.Errorf("TerminalActionFor(isHuman=%v, hasLiveAgent=%v) = %v, want %v",
				c.isHuman, c.hasLiveAgent, got, c.want)
		}
	}
}

func TestEscalationTierString(t *testing.T) {
	// These tokens are a wire format (stored in markers); pin them.
	cases := map[EscalationTier]string{
		TierNone: "none", TierWarn: "warn", TierEscalate: "escalate", TierTerminal: "terminal",
	}
	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", tier, got, want)
		}
	}
}
