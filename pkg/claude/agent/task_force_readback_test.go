package agent

import (
	"bytes"
	"strings"
	"testing"
)

// JOH-346: the `task-force status` renderer + the shared liveness classifier.
// These are pure unit tests over the wire-shape structs — no daemon — that lock
// in the CLI-shape output the flow tests only assert at the endpoint level.

// TestForceMemberLiveness_MatchesDashboard locks the classifier to the dashboard
// force block's semantics (render.js forceMemberLiveness): offline → dead;
// online + literal "idle" → idle; anything else in flight → working.
func TestForceMemberLiveness_MatchesDashboard(t *testing.T) {
	cases := []struct {
		name   string
		online bool
		status string
		want   string
	}{
		{"offline is dead regardless of frozen status", false, "idle", "dead"},
		{"offline exited is dead", false, "exited", "dead"},
		{"online idle is idle", true, "idle", "idle"},
		{"online working is working", true, "working", "working"},
		{"online awaiting_* is working", true, "awaiting_input", "working"},
		{"online main_agent_idle is working (not literally idle)", true, "main_agent_idle", "working"},
		{"online unreported is working", true, "", "working"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := forceMemberLiveness(c.online, c.status); got != c.want {
				t.Fatalf("forceMemberLiveness(%v, %q) = %q, want %q", c.online, c.status, got, c.want)
			}
		})
	}
}

// TestRhythmEnabledLabel distinguishes an auto-paused rhythm (a retire that
// emptied the group) from a human-paused one — the JOH-346 operator affordance.
func TestRhythmEnabledLabel(t *testing.T) {
	if got := rhythmEnabledLabel(cronJobJSON{Enabled: true}); got != "enabled" {
		t.Fatalf("enabled = %q", got)
	}
	if got := rhythmEnabledLabel(cronJobJSON{Enabled: false}); got != "disabled" {
		t.Fatalf("hand-disabled = %q", got)
	}
	got := rhythmEnabledLabel(cronJobJSON{Enabled: false, DisabledReason: "group-retired"})
	if got != "disabled (auto: group-retired)" {
		t.Fatalf("auto-disabled = %q, want the group-retired marker", got)
	}
}

// TestPrintTaskForceStatus_FullBlock renders a live force and asserts every
// section the dashboard force block carries surfaces on the CLI: mission +
// provenance, the phase map + a transition, the per-role liveness rollup with a
// context %%, pending waves, and the rhythms (incl. an auto-disabled one).
func TestPrintTaskForceStatus_FullBlock(t *testing.T) {
	v := &taskForceStatusView{
		Group:          "raid",
		Mission:        "Neutralise the auth backlog.",
		SourceTemplate: "recon",
		Online:         2,
		Total:          3,
		Process: &processStateView{
			CurrentPhase: "recon",
			PhaseIndex:   0,
			PhaseCount:   2,
			Phases: []processPhaseView{
				{Name: "recon", Roles: []string{"lead"}, Current: true},
				{Name: "strike", Roles: []string{"dev"}},
			},
			Transitions: []processTransitionView{
				{From: "", To: "recon", Actor: "human", At: "2026-07-04T00:00:00Z"},
			},
		},
		Waves:             &waveStatusView{CurrentWave: 1, TotalWaves: 2, PendingWaves: 1, PendingAgents: 1},
		MembersAccessible: true,
		Members: []forceMemberView{
			{ConvID: "c-lead", Title: "lead-1", Role: "lead", Online: true, Status: "working", HasSnapshot: true, ContextPct: 42},
			{ConvID: "c-dev", Title: "dev-1", Role: "dev", Online: false, Status: "exited"},
		},
		Rhythms: []cronJobJSON{
			{ID: 1, Name: "raid-checkin", IntervalSeconds: 1800, Enabled: true, TargetRole: "lead"},
			{ID: 2, Name: "raid-standup", IntervalSeconds: 3600, Enabled: false, DisabledReason: "group-retired"},
		},
	}
	var buf bytes.Buffer
	printTaskForceStatus(&buf, v)
	out := buf.String()

	wants := []string{
		`Task force "raid"`,
		"Neutralise the auth backlog.",
		"Template:  recon",
		"2 live / 3 total",
		"Phase:     1/2 recon",
		"▸ 1. recon",
		"(start) → recon",
		"by human",
		"Roles:",
		"● lead-1 (42%)", // working, with context %
		"✕ dev-1",        // offline → dead glyph
		"(role: lead)",   // rhythm role annotation
		"Waves:     1 pending (of 2)",
		"raid-checkin",
		"enabled",
		"raid-standup",
		"disabled (auto: group-retired)",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("status output missing %q\n--- full output ---\n%s", w, out)
		}
	}
}

// TestPrintTaskForceStatus_DormantAndDegraded covers the two graceful paths: a
// stood-down force reads as dormant, and a caller without context access sees a
// clear note instead of the liveness rollup (still getting mission/provenance).
func TestPrintTaskForceStatus_DormantAndDegraded(t *testing.T) {
	v := &taskForceStatusView{
		Group:             "dusk",
		Mission:           "Wind me down.",
		SourceTemplate:    "recon",
		Online:            0,
		Total:             0,
		Dormant:           true,
		MembersAccessible: false,
		Rhythms:           []cronJobJSON{},
	}
	var buf bytes.Buffer
	printTaskForceStatus(&buf, v)
	out := buf.String()

	for _, w := range []string{
		"dormant — no live members",
		"Wind me down.",
		"Template:  recon",
		"liveness detail needs context access",
		"Rhythms:   (none)",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("dormant/degraded output missing %q\n--- full output ---\n%s", w, out)
		}
	}
}
