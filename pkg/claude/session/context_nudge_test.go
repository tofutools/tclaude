package session

import (
	"strings"
	"testing"
)

// nextNudgeTarget is the ladder-position helper for the context-nudge
// Stop-hook path. Pin the rule shape and the "below min skip" path.
func TestNextNudgeTarget(t *testing.T) {
	cases := []struct {
		name           string
		pct            float64
		minPct, stepPct int
		want           int
	}{
		{"below min skips", 25, 30, 10, 0},
		{"at min fires", 30, 30, 10, 30},
		{"just past min fires at min", 35, 30, 10, 30},
		{"between steps fires at lower", 49, 30, 10, 40},
		{"on step fires at that step", 50, 30, 10, 50},
		{"high pct fires at floor", 85, 30, 10, 80},
		{"caps at 90", 92, 30, 10, 90},
		{"caps at 90 even beyond", 99, 30, 10, 90},
		{"different ladder min=50 step=20", 70, 50, 20, 70},
		{"different ladder caps", 95, 50, 20, 90},
		{"zero step → 0 (invalid config)", 50, 30, 0, 0},
		{"zero min → 0 (invalid config)", 50, 0, 10, 0},
		{"negative step → 0", 50, 30, -10, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextNudgeTarget(c.pct, c.minPct, c.stepPct)
			if got != c.want {
				t.Errorf("nextNudgeTarget(%v, %d, %d) = %d, want %d",
					c.pct, c.minPct, c.stepPct, got, c.want)
			}
		})
	}
}

// formatContextNudgeMessage is the text typed into the pane. Pin the
// shape: includes the threshold % and the suggestion (so a future
// reader of the agent transcript can tell it's a context nudge).
func TestFormatContextNudgeMessage(t *testing.T) {
	msg := formatContextNudgeMessage(50)
	if !strings.HasPrefix(msg, "[system: ") {
		t.Errorf("message must start with [system: ...]; got %q", msg)
	}
	if !strings.Contains(msg, "50%") {
		t.Errorf("message must include the threshold; got %q", msg)
	}
	if !strings.Contains(msg, "/reincarnate") {
		t.Errorf("message must suggest /reincarnate; got %q", msg)
	}
}
