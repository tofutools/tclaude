package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_HarnessLineWired guards the per-agent harness/model
// line — "CC · Opus 4.8" under the row's dot/focus/cog cluster — plus
// its appearance in the status-dot tooltip. The pieces span three files
// (helpers.js builds + exports it, render.js wires it into the member
// cell, dashboard.css styles it); a rename in one silently breaks the
// feature in the browser, and the repo has no JS test runner, so this
// asserts on the embedded concatenation at `go test ./...`.
//
// The model itself comes from state.model — surfaced by the dashboard
// snapshot from the sessions.model column the statusline hook records.
func TestDashboardHTML_HarnessLineWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// helpers.js: the builders are defined, read state.model, and are
	// exported for render.js.
	must("function harnessLine(m)", "harnessLine helper is defined")
	must("function harnessModel(m)", "harnessModel helper (tooltip text) is defined")
	must("m.state.model", "the line reads the model off the agent's state")
	must("agentStatusDot, harnessLine,", "harnessLine is exported from helpers.js")

	// render.js: wired into the member control cell — same column as the
	// dot/actions, NOT a new <td>.
	must("${agentStatusDot(m)}${actions}</div>${harnessLine(m)}", "harnessLine renders in the agent-ctl cell")

	// Status-dot tooltip surfaces the harness+model on hover (the brief's
	// second ask).
	must("running on ${hm}", "the status-dot tooltip appends the harness/model")

	// CSS: the line and its two spans are styled.
	must(".agent-harness", "harness line has a style rule")
	must(".harness-model", "model span has a style rule")

	// The harness is a frontend constant (only Claude Code exists), not a
	// DB column — the chip says "CC".
	must("const HARNESS_SHORT = 'CC'", "harness short label is the CC constant")
}
