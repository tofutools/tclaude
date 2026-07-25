package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_WhatIfCostWired guards the WHAT-IF cost feature, whose
// pieces span dashboard.html, dashboard.css and four JS modules. A rename in
// any one file would silently break the feature in the browser, and the
// behavioural suites in jstest/ mount islands one at a time and so cannot see
// across that seam; this asserts on the embedded concatenation instead.
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

	// HarnessLine reads virtual cost and emits a hypothetical span,
	// trailing the real cost token, with a ≈ prefix so it never reads as real.
	must("Number(state.virtual_cost_usd || 0)", "HarnessLine reads the WHAT-IF cost off the agent's state")
	must("harness-cost-whatif", "the WHAT-IF cost has its own span class")
	must("${virtualCost > 0 ? html`<span class=\"harness-cost harness-cost-whatif\"", "the WHAT-IF token sits between the real cost and the remote indicator")
	must("`≈$${virtualCost.toFixed(2)}`", "WHAT-IF cost is prefixed ≈ to read as an estimate")

	// CSS: the WHAT-IF span is hidden unless body.cost-whatif; the 💲 toggle
	// suppresses the badge; the Costs nav button + section hide on body.hide-costs.
	must("body.cost-whatif .agent-harness .harness-cost-whatif", "WHAT-IF cost shows only in WHAT-IF mode")
	must("body.agent-cost-hidden .agent-harness .harness-cost", "the 💲 toggle hides the per-agent cost badge")
	must(`body.hide-costs nav [data-tab="costs"]`, "the Costs nav button hides when there's nothing to show")
	must("body.hide-costs #tab-costs", "the Costs section hides alongside its nav button")
	must(".cost-whatif-banner", "the WHAT-IF banner has a style rule")

	// dashboard.html: the banner, the Config-tab opt-in checkbox, the toggle.
	must(`id="costs-whatif-banner"`, "the WHAT-IF banner element exists in the Costs tab")
	// That the caveat renders *above* the controls it qualifies, and that its
	// two sentences keep their space, are DOM facts rather than source-order
	// ones — asserted on the mounted island in
	// jstest/costs-island.test.mjs ("keeps its cross-line word gaps").
	must(`id="cfg-cost-show-on-subscription"`, "the Config tab carries the show-on-subscription checkbox")
	must(`id="groups-cost-toggle"`, "the Groups filter bar carries the 💲 cost toggle")

	// Costs state/island: visibility is driven off the snapshot's server flags, with a
	// stranded-active-tab fallback to Groups.
	must("snap?.cost_tab_visible", "visibility reads the server's cost_tab_visible flag")
	must("snap?.cost_tab_whatif", "WHAT-IF mode reads the server's cost_tab_whatif flag")
	must("'hide-costs'", "refresh toggles body.hide-costs")

	// Costs always fetches one mixed response, whose per-row kind metadata
	// drives the per-row WHAT-IF marker — a dim ⚠ back to the banner rather
	// than a repeat of its caveat; the toggle is bound and persisted.
	must("what_if_total_usd", "Costs state carries the hypothetical subtotal")
	must(`<a class="cost-whatif-mark" href="#costs-whatif-banner"`,
		"Costs table marks hypothetical rows with a ⚠ pointing at the banner")
	must(".cost-whatif-mark {", "the per-row WHAT-IF marker has a style rule")
	// The marker's click-through spans all three files, and the jstest suites
	// mount JS without the stylesheet, so this seam is only visible here: the
	// class the handler adds, the rule that animates it, and the shared
	// keyframes that rule borrows from the Access queue.
	must("classList.add('cost-whatif-flash')", "clicking the marker flashes the banner it scrolled to")
	must(".cost-whatif-flash { animation: access-attn", "the flash animates via the shared attention pulse")
	must("@keyframes access-attn {", "that shared pulse still exists to borrow")
	must(".cost-whatif-banner:focus", "the focused banner shows where the jump landed")
	must("segment.kind === 'what_if'", "Costs chart tooltips label hypothetical segments")
	must("function bindCostDisplayToggle(", "the 💲 toggle is bound")
	must("'agent-cost-hidden'", "the toggle drives body.agent-cost-hidden")
	must("from './cost-display-toggle.js'", "bindCostDisplayToggle is wired from its shell module")

	// config.js: the opt-in round-trips through the cost block.
	must("cost.show_on_subscription", "config.js reads/writes the opt-in")
}
