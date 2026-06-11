package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_CostBadgeWired guards the per-agent API-cost badge —
// "$0.42" in the status column, right of the state pill. The pieces
// span three files (helpers.js builds + exports costBadge, render.js
// wires it into the state cell, dashboard.css styles it); a rename in
// one silently breaks the feature in the browser, and the repo has no
// JS test runner, so this asserts on the embedded concatenation at
// `go test ./...`.
//
// The cost itself comes from state.cost_usd — surfaced by the dashboard
// snapshot from the sessions.cost_usd column the statusline hook records
// ONLY on API/enterprise pricing (no subscription rate-limit buckets).
// costBadge returns '' for 0, so subscription rows render no badge and
// the status column looks exactly as before.
func TestDashboardHTML_CostBadgeWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// helpers.js: the builder is defined, reads state.cost_usd, gates on
	// nonzero, and is exported for render.js.
	must("function costBadge(state)", "costBadge helper is defined")
	must("state.cost_usd", "the badge reads the cost off the agent's state")
	must("if (!(cost > 0)) return '';", "zero/absent cost renders nothing")
	must("statePill, costBadge,", "costBadge is exported from helpers.js")

	// render.js: wired into the state cell, right of the state pill.
	must("${statePill(state, m.online)}\n                  ${costBadge(state)}",
		"costBadge renders right of the state pill in the state cell")

	// Sub-cent costs show as "<1¢", never a lying "$0.00".
	must("cost >= 0.005 ? '$' + cost.toFixed(2) : '<1¢'",
		"two-decimal dollar format with a sub-cent floor")

	// The tooltip carries the precise figure and names the pricing mode.
	must("API cost this session", "tooltip explains what the figure is")

	// CSS: the badge is styled.
	must(".cost-badge", "cost badge has a style rule")
}
