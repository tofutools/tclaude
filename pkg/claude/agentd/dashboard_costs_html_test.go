package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_TopBarTotalCostWired guards the top bar's cost
// token: render.js reads usage.total_cost_usd and usage.today_cost_usd
// off the snapshot and renders "$X.XX (mtd)" — with "$Y.YY (today)"
// ahead of it when today's spend is a distinct slice — next to or
// instead of the subscription windows, so an API-billing account sees
// its spend where "usage: n/a" used to sit. The token links to the
// Costs tab (costs.js). Pieces span render.js + costs.js + dashboard.css
// and the repo has no JS test runner, so this asserts on the embedded
// concatenation at `go test ./...`.
func TestDashboardHTML_TopBarTotalCostWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// render.js: the token reads both snapshot fields and only renders
	// for nonzero cost, with the harness-line sub-cent floor.
	must("u.total_cost_usd", "renderUsage reads the snapshot's month-to-date total")
	must("u.today_cost_usd", "renderUsage reads the snapshot's today total")
	must("costTokenHTML", "the cost token has its own builder")
	must("cost >= 0.005 ? '$' + cost.toFixed(2) : '<1¢'",
		"two-decimal dollar format with a sub-cent floor")

	// The today figure is rendered ahead of mtd, and suppressed when it
	// would duplicate the mtd within a cent (a single-day month).
	must("amt(today, 'today')", "today's figure rendered with its own label")
	must("amt(mtd, 'mtd')", "month-to-date figure rendered with its own label")
	must("today > 0 && mtd - today >= 0.005",
		"today suppressed when it equals mtd within a cent")

	// The token links to the Costs tab: render.js tags it, costs.js
	// delegates the click to the nav button.
	must(`data-goto-tab="costs"`, "cost token tagged as a Costs-tab link")
	must(`nav button[data-tab="costs"]').click()`,
		"costs.js opens the tab via the nav button on a token click")

	// The no-data state is unchanged: neither windows nor cost → n/a.
	must("'usage: n/a'", "graceful n/a fallback still present")

	// CSS: the amount is styled in the same money-green as the
	// per-agent cost tokens, and the clickable token gets a pointer.
	must(".ucost-amt", "top-bar cost amount has a style rule")
	must("#usage .ucost { cursor: pointer", "clickable cost token has a pointer cursor")
}

// TestDashboardHTML_CostsTabWired guards the Costs tab end to end
// across dashboard.html (nav button + section), costs.js (fetch, span
// selector, projection, chart, table), dashboard.js (module wired in)
// and dashboard.css (bar styling) — a rename in any one file silently
// breaks the tab in the browser.
func TestDashboardHTML_CostsTabWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// dashboard.html: the tab exists and carries the span selector,
	// chart, summary and table mount points.
	must(`data-tab="costs"`, "Costs nav button present")
	must(`id="tab-costs"`, "Costs tab section present")
	must(`id="costs-spans"`, "span selector bar present")
	must(`id="costs-chart"`, "chart mount point present")
	must(`id="costs-summary"`, "summary mount point present")
	must(`id="costs-table"`, "breakdown table mount point present")
	must(`data-span="month"`, "current-month span option present")
	must(`data-span="30d"`, "last-30d span option present")

	// costs.js: fetches the endpoint, projects the month from elapsed
	// weekdays (weekends excluded), renders chart before table.
	must("/api/costs?from=", "costs.js fetches the costs endpoint")
	must("function monthProjection", "month projection implemented")
	must("isWeekendKey", "weekends excluded from the projection")
	must("bindCostsTab", "tab binder exported")

	// dashboard.js: the module is actually imported and bound.
	must("from './costs.js'", "costs.js wired into the entrypoint")
	must("bindCostsTab();", "tab binder invoked at startup")

	// dashboard.css: recorded vs projected bars are distinguishable.
	must(".cost-bar", "bar style rule present")
	must(".cost-col.projected .cost-bar", "projected bars styled hollow")
	must(".cost-col.weekend .cost-bar", "weekend bars dimmed")
}
