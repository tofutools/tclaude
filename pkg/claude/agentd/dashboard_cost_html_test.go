package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_CostInHarnessLineWired guards the per-agent API-cost
// token — "$0.42" trailing the harness/model line ("CC · O4.8 1M high
// $0.42") under the row's dot/focus/cog cluster. The pieces span two
// files (helpers.js builds it inside harnessLine, dashboard.css styles
// it); a rename in one silently breaks the feature in the browser, and
// the repo has no JS test runner, so this asserts on the embedded
// concatenation at `go test ./...`.
//
// The cost itself comes from state.cost_usd — surfaced by the dashboard
// snapshot from the sessions.cost_usd column the statusline hook records
// ONLY on API/enterprise pricing (no subscription rate-limit buckets).
// harnessLine omits the token for 0, so subscription rows render exactly
// as before.
func TestDashboardHTML_CostInHarnessLineWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// HarnessLine reads the cost off the agent's state and
	// emits its own styled span, trailing the effort token.
	must("Number(state.cost_usd || 0)", "HarnessLine reads the cost off the agent's state")
	must("harness-cost", "the cost token has its own span")
	must("${cost > 0 ? html`<span class=\"harness-cost\"", "the cost token trails the effort token in the line")

	// Zero/absent cost renders no token at all.
	must("cost > 0 ? html", "the token is gated on nonzero cost")

	// Sub-cent costs show as "<1¢", never a lying "$0.00".
	must("cost >= 0.005 ? `$${cost.toFixed(2)}` : '<1¢'",
		"two-decimal dollar format with a sub-cent floor")

	// The tooltip carries the precise figure and names the pricing mode.
	must("API cost this session", "tooltip explains what the figure is")

	// CSS: the token is styled.
	must(".harness-cost", "cost token has a style rule")
}
