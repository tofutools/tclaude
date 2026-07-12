package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_WhatIfCostWired guards the WHAT-IF cost feature, whose
// pieces span dashboard.html, dashboard.css and four JS modules. The repo has
// no JS test runner, so a rename in any one file would silently break the
// feature in the browser; this asserts on the embedded concatenation at
// `go test ./...`.
//
// The feature: on a subscription the Costs tab + per-agent cost badge auto-hide
// (no real spend to show); enabling cost.show_on_subscription reveals them in
// WHAT-IF mode — the estimated pay-per-token-equivalent cost (virtual_cost_usd),
// flagged hypothetical. A 💲 Groups-tab toggle shows/hides the per-agent badge.
func TestDashboardHTML_WhatIfCostWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// helpers.js: harnessLine reads virtual cost and emits a hypothetical span,
	// trailing the real cost token, with a ≈ prefix so it never reads as real.
	must("m.state.virtual_cost_usd", "harnessLine reads the WHAT-IF cost off the agent's state")
	must("harness-cost-whatif", "the WHAT-IF cost has its own span class")
	must("+ effortEl + costEl + whatifEl + remoteEl", "the WHAT-IF token sits between the real cost and the remote indicator")
	must("'≈$' + vcost.toFixed(2)", "WHAT-IF cost is prefixed ≈ to read as an estimate")

	// CSS: the WHAT-IF span is hidden unless body.cost-whatif; the 💲 toggle
	// suppresses the badge; the Costs nav button + section hide on body.hide-costs.
	must("body.cost-whatif .agent-harness .harness-cost-whatif", "WHAT-IF cost shows only in WHAT-IF mode")
	must("body.agent-cost-hidden .agent-harness .harness-cost", "the 💲 toggle hides the per-agent cost badge")
	must(`body.hide-costs nav [data-tab="costs"]`, "the Costs nav button hides when there's nothing to show")
	must("body.hide-costs #tab-costs", "the Costs section hides alongside its nav button")
	must(".cost-whatif-banner", "the WHAT-IF banner has a style rule")

	// dashboard.html: the banner, the Config-tab opt-in checkbox, the toggle.
	must(`id="costs-whatif-banner"`, "the WHAT-IF banner element exists in the Costs tab")
	must(`id="cfg-cost-show-on-subscription"`, "the Config tab carries the show-on-subscription checkbox")
	must(`id="groups-cost-toggle"`, "the Groups filter bar carries the 💲 cost toggle")

	// Costs state/island: visibility is driven off the snapshot's server flags, with a
	// stranded-active-tab fallback to Groups.
	must("snap?.cost_tab_visible", "visibility reads the server's cost_tab_visible flag")
	must("snap?.cost_tab_whatif", "WHAT-IF mode reads the server's cost_tab_whatif flag")
	must("'hide-costs'", "refresh toggles body.hide-costs")

	// costs.js: WHAT-IF mode appends ?whatif=1 and shows the banner; the toggle
	// is bound and persisted.
	must("current.whatif ? '&whatif=1'", "Costs actions read WHAT-IF mode from state")
	must("'&whatif=1'", "the Costs tab fetches the virtual figures in WHAT-IF mode")
	must("function bindCostDisplayToggle(", "the 💲 toggle is bound")
	must("'agent-cost-hidden'", "the toggle drives body.agent-cost-hidden")
	must("from './cost-display-toggle.js'", "bindCostDisplayToggle is wired from its shell module")

	// config.js: the opt-in round-trips through the cost block.
	must("cost.show_on_subscription", "config.js reads/writes the opt-in")
}
