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
	must("data.first_day", "projection anchors the weekday average at the first-ever costed day")
	must("bindCostsTab", "tab binder exported")

	// dashboard.js: the module is actually imported and bound.
	must("from './costs.js'", "costs.js wired into the entrypoint")
	must("bindCostsTab();", "tab binder invoked at startup")

	// dashboard.css: recorded vs projected bars are distinguishable.
	must(".cost-bar", "bar style rule present")
	must(".cost-col.projected .cost-bar", "projected bars styled hollow")
	must(".cost-col.weekend .cost-bar", "weekend bars dimmed")

	// Y axis: costs.js computes a nice scale top and renders tick
	// labels + gridlines; css positions both on the same percentages.
	must("function niceCeil", "nice Y-axis scale top computed")
	must("cost-ytick", "Y-axis tick labels rendered")
	must(".cost-gridline", "gridlines styled")

	// Hover tooltip: instant CSS tooltip off data-tip (not the
	// delayed native title), and only on columns with spend.
	must("data-tip", "tooltip attribute rendered on nonzero columns")
	must(".cost-col[data-tip]:hover::after", "instant CSS tooltip rule present")

	// Breakdown table: the per-agent model column.
	must("<th>Model</th>", "model column header present")
	must("a.model", "model field rendered from the API row")

	// Continued-conversation marker: a multi-day conversation splits into
	// one row per day, and the earlier-day slices carry `continued`,
	// rendered with a ↩ marker styled by .cost-cont.
	must("a.continued", "continuation flag read from the API row")
	must("↩", "continuation marker glyph rendered")
	must(".cost-cont", "continuation marker styled")
}

// TestDashboardHTML_CostsFillEmptyWeekdaysWired guards the Costs tab's
// "fill empty weekdays" toggle across dashboard.html (the checkbox),
// costs.js (the persisted flag, the leading-weekday fill in the month
// projection, the chart + summary switch) and dashboard.css (the
// disabled-toggle styling). The toggle turns the month figure from
// "projected current month cost" (mid-month start skews it low) into
// "projected average month cost" by filling the empty weekdays before
// the first run this month at the per-weekday average. Pieces span
// three files with no JS test runner, so this asserts on the embedded
// concatenation at `go test ./...`.
func TestDashboardHTML_CostsFillEmptyWeekdaysWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// dashboard.html: the checkbox + its label live in the span bar.
	must(`id="costs-fill-weekdays"`, "fill-empty-weekdays checkbox present")
	must(`id="costs-fill-weekdays-label"`, "checkbox label present (toggled disabled off the month span)")
	must("fill empty weekdays", "checkbox label text present")

	// costs.js: the toggle is persisted server-side via dashPrefs and
	// passed into the projection.
	must("tclaude.dash.costs.fillEmptyWeekdays", "toggle persisted under its own pref key")
	must("monthProjection(data, fillEmptyWeekdays)", "projection receives the fill flag")
	must("function monthProjection(data, fillEmpty)", "projection takes the fill flag")

	// costs.js: the projection builds the leading-weekday fill and the
	// chart renders those columns as projected bars; the summary label
	// switches to the average-month wording when on.
	must("leadingFill", "projection computes the leading empty-weekday fill")
	must("Projected avg month total", "summary switches to the average-month label when filling")

	// costs.js: the toggle is inert (disabled) on the non-month spans
	// that have no projection.
	must("function syncFillToggle", "toggle disabled off the month span")

	// dashboard.css: a disabled toggle is visibly dimmed.
	must(".filter-bar label.filter-toggle.disabled", "disabled toggle styled")
}

// The cost display multiplier is editable in two places — live on the
// Costs tab and persisted via the Config tab — both backed by
// /api/cost-factor. Pin the wiring so a rename of an id or endpoint
// breaks loudly.
func TestDashboardHTML_CostFactorWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// dashboard.html: the live input on the Costs tab + the Config tab field.
	must(`id="costs-factor"`, "live cost-factor input on the Costs tab")
	must(`id="cfg-cost-factor"`, "cost-factor field on the Config tab")

	// costs.js: load on activation, persist + reload on edit, all via
	// /api/cost-factor.
	must("/api/cost-factor", "costs.js talks to the cost-factor endpoint")
	must("function loadCostFactor", "factor loaded when the tab opens")
	must("function saveCostFactor", "factor persisted + costs reloaded on edit")

	// config.js: round-trips cost.estimate_factor.
	must("estimate_factor", "config.js round-trips the cost.estimate_factor key")
}
