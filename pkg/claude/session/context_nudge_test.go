package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// nextNudgeTarget is the ladder-position helper for the context-nudge
// Stop-hook path. Pin the rule shape and the "below min skip" path.
func TestNextNudgeTarget(t *testing.T) {
	cases := []struct {
		name            string
		pct             float64
		minPct, stepPct int
		want            int
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
			assert.Equalf(t, c.want, got, "nextNudgeTarget(%v, %d, %d)", c.pct, c.minPct, c.stepPct)
		})
	}
}

// formatContextNudgeMessage is typed directly into the pane. Pin the envelope
// so a future transcript still identifies it as a system-generated reminder.
func TestFormatContextNudgeMessage(t *testing.T) {
	msg := formatContextNudgeMessage(50)
	assert.Truef(t, strings.HasPrefix(msg, "[system: "), "message must start with [system: ...]; got %q", msg)
	assert.Containsf(t, msg, "50%", "message must include the threshold; got %q", msg)
	assert.Containsf(t, msg, "/reincarnate", "message must suggest /reincarnate; got %q", msg)
}
