package processexec

import (
	"testing"
	"time"
)

func TestDecideContactScenarios(t *testing.T) {
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	live := ContactSnapshot{
		Cadence: 5 * time.Minute, Budget: 3, Used: 1,
		NextContactAt: base, LastContactedAt: base.Add(-5 * time.Minute),
	}
	cases := []struct {
		name     string
		snapshot ContactSnapshot
		activity Activity
		now      time.Time
		want     ContactDecision
	}{
		{
			name: "idle before due", snapshot: live, now: base.Add(-time.Minute),
			want: ContactDecision{},
		},
		{
			name: "due nudges", snapshot: live, now: base,
			want: ContactDecision{Send: ContactSendNudge},
		},
		{
			name: "exhausted budget escalates",
			snapshot: func() ContactSnapshot { s := live; s.Used = 3; return s }(),
			now:  base,
			want: ContactDecision{Send: ContactSendEscalate},
		},
		{
			name: "escalated stays silent",
			snapshot: func() ContactSnapshot {
				s := live
				s.Used, s.EscalatedAt, s.NextContactAt = 3, base.Add(-time.Hour), time.Time{}
				return s
			}(),
			now:  base,
			want: ContactDecision{},
		},
		{
			name: "recovery resets and defers the due nudge",
			snapshot: func() ContactSnapshot {
				s := live
				s.Used, s.EscalatedAt = 3, base.Add(-time.Hour)
				return s
			}(),
			activity: Activity{Recovered: true, At: base.Add(-time.Minute)},
			now:      base,
			want:     ContactDecision{Reset: true, ResetAt: base.Add(-time.Minute)},
		},
		{
			name:     "stale recovery is ignored",
			snapshot: func() ContactSnapshot { s := live; s.LastRecoveredAt = base; return s }(),
			activity: Activity{Recovered: true, At: base.Add(-time.Minute)},
			now:      base,
			want:     ContactDecision{Send: ContactSendNudge},
		},
		{
			name:     "human interaction latches without immediate pause",
			snapshot: func() ContactSnapshot { s := live; s.NextContactAt = base.Add(time.Hour); return s }(),
			activity: Activity{HumanInteracted: true, At: base},
			now:      base,
			want:     ContactDecision{LatchAt: base},
		},
		{
			name: "latched interaction pauses after grace and blocks the send",
			snapshot: func() ContactSnapshot {
				s := live
				s.HumanInteractedAt = base.Add(-ContactHumanPreemptionGrace)
				return s
			}(),
			now:  base,
			want: ContactDecision{Pause: true},
		},
		{
			name: "automated delivery clears the latch and releases the pause",
			snapshot: func() ContactSnapshot {
				s := live
				s.HumanInteractedAt = base.Add(-time.Minute)
				s.Paused, s.PauseReason = true, ContactPauseReasonHumanPreemption
				return s
			}(),
			activity: Activity{AutomatedDelivery: true, At: base},
			now:      base,
			want:     ContactDecision{ClearLatch: true, Send: ContactSendNudge},
		},
		{
			name: "operator pause is never released by delivery metadata",
			snapshot: func() ContactSnapshot {
				s := live
				s.Paused, s.PauseReason = true, "performer observed"
				return s
			}(),
			activity: Activity{AutomatedDelivery: true, At: base},
			now:      base,
			want:     ContactDecision{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideContact(tc.snapshot, tc.activity, tc.now)
			if got != tc.want {
				t.Fatalf("decision = %+v, want %+v", got, tc.want)
			}
		})
	}
}
