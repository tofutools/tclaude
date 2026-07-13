package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_TopBarTotalCostWired guards the top bar's cost
// token: render.js reads usage.total_cost_usd and usage.today_cost_usd
// off the snapshot and renders "$X.XX (mtd)" — with "$Y.YY (today)"
// ahead of it whenever anything was spent today — next to or
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

	// The today figure is rendered ahead of mtd whenever anything was spent
	// today — including when it equals mtd (e.g. the first of the month),
	// so the "(today)" figure never silently vanishes.
	must("amt(today, 'today')", "today's figure rendered with its own label")
	must("amt(mtd, 'mtd')", "month-to-date figure rendered with its own label")
	must("if (today > 0) parts.push(amt(today, 'today'))",
		"today shown whenever anything was spent today, even when it equals mtd")

	// The token links to the Costs tab: render.js tags it, the island
	// delegates the click to the nav button.
	must(`data-goto-tab="costs"`, "cost token tagged as a Costs-tab link")
	must(`document.querySelector('nav [data-tab="costs"]')?.click()`,
		"Costs island opens the tab via the nav button on a token click")

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
	must(`data-span=${span.key}`, "fixed span options rendered by Preact")
	must(`{ key: '30d', label: 'Last 30d'`, "last-30d span declared")

	// costs.js: fetches the endpoint, projects the month from elapsed
	// weekdays (weekends excluded), renders chart before table.
	must("/api/costs?from=", "costs.js fetches the costs endpoint")
	must("function monthProjection", "month projection implemented")
	must("isWeekendKey", "weekends excluded from the projection")
	must("data.first_day", "projection anchors the weekday average at the first-ever costed day")
	must("mountCostsFeature", "guarded Costs feature mount exported")

	// dashboard.js: the module is actually imported and bound.
	must("mountCostsFeature(),", "Costs island mounted in the concurrent bounded-feature group")

	// dashboard.css: recorded vs projected bars are distinguishable.
	must(".cost-bar", "bar style rule present")
	must(".cost-col.projected .cost-bar", "projected bars styled hollow")
	must(".cost-col.weekend .cost-bar", "weekend bars dimmed")

	// Stacked per-harness chart: recorded columns split into coloured
	// segments from the per-agent rows; the harness filter doubles as the
	// legend, and the hover tooltip itemises the split over a ruled total.
	must("function mountImperativeCostChart", "recorded columns rendered by the imperative chart boundary")
	must("function dailyBreakdown", "per-day per-harness split derived from the agent rows")
	must("function tooltipRows", "per-harness hover legend builder present")
	must(".cost-seg-h0", "harness palette defined (h0 = the historical money-green)")
	must(".cost-col[data-tip]:hover .cost-seg", "stacked-segment hover highlight present")
	must(".cost-legend-sw", "harness filter carries a colour-legend swatch")
	must(".cost-tip-total", "tooltip's ruled total row styled")
	must("body.wizard .cost-seg-h1", "wizard mode themes the stacked segments")

	// Y axis: costs.js computes a nice scale top and renders tick
	// labels + gridlines; css positions both on the same percentages.
	must("function niceCeil", "nice Y-axis scale top computed")
	must("cost-ytick", "Y-axis tick labels rendered")
	must(".cost-gridline", "gridlines styled")

	// Hover tooltip: cursor-following tooltip off data-tip (not the
	// delayed native title), and only on columns with spend. The tip body
	// is a body-level .cost-tip element positioned by bindCostsChartTip;
	// the bar-highlight stays a pure CSS :hover rule.
	must("data-tip", "tooltip attribute rendered on nonzero columns")
	must(".cost-col[data-tip]:hover .cost-bar", "hover bar-highlight rule present")
	must(".cost-tip", "cursor-following tooltip element styled")
	must("host.addEventListener('mousemove', move)", "cursor-following tooltip wired")

	// Breakdown table: the per-agent harness + model columns, now rendered through
	// the sortable-header builder rather than a static <th>.
	must("{ label: 'Harness', sort: 'harness'", "harness column defined in the sortable header set")
	must("agent.harness", "harness field rendered from the API row")
	must("{ label: 'Model', sort: 'model'", "model column defined in the sortable header set")
	must("agent.model", "model field rendered from the API row")

	// Sortable headers: clickable columns with a direction arrow, default
	// activity/desc (which reproduces the server's recency order), wired on
	// a delegated click — the same affordance as the Audit tab.
	must("function SortHeader", "sortable Preact header component present")
	must("th.cost-sort", "header click target class rendered + bound")
	must("function sortCostAgents", "client-side column sort implemented")
	must("state.cycleSort(key)", "header sort action wired")
	must("#costs-table th.cost-sort { cursor: pointer", "sortable headers styled clickable")

	// Breakdown filter: harness checkboxes plus a client-side text narrowing of
	// the table (matches name / id / harness / model), with the matched/all
	// count chip and clear button.
	must(`id="filter-costs-harnesses"`, "harness filter mount present")
	must("function HarnessFilter", "harness checkbox component wired")
	must("tclaude.dash.costs.harnesses", "harness filter persisted")
	// The harness subset narrows the whole tab, not just the table: the
	// chart/summary/projection render from the filtered derivation, and a
	// checkbox toggle re-paints all three panes from the payload in hand.
	must("function filterCostData", "harness subset narrows the chart/summary totals")
	must("state.toggleHarness(harness)", "checkbox toggle re-derives chart + summary + table without refetch")
	must(`id="filter-costs"`, "breakdown filter input present")
	must(`id="filter-costs-count"`, "filter match-count chip present")
	must(`id="filter-costs-clear"`, "filter clear button present")
	must("state.setQuery(event.currentTarget.value)", "filter input wired")
	must("No agents match the filter.", "empty-after-filter state rendered")

	// Continued-conversation marker: a multi-day conversation splits into
	// one row per day, and the earlier-day slices carry `continued`,
	// rendered with a ↩ marker styled by .cost-cont.
	must("agent.continued", "continuation flag read from the API row")
	must("↩", "continuation marker glyph rendered")
	must(".cost-cont", "continuation marker styled")

	// Multi-day chain: a conversation with more than one slice tags its
	// rows so the current generation (the latest-day head) reads as the
	// live tip of a chain and hovering any row highlights the whole set.
	must("slices[agent.conv_id]", "rows-per-conversation counted to detect multi-day chains")
	must("↳", "chain-head marker glyph rendered on the latest day")
	must(".cost-head", "chain-head marker styled")
	must(`data-conv="`, "chain rows tagged with their shared conv id")
	must("setHovered(event.target.closest('tr[data-conv]')", "chain hover-highlight wired")
	must(".cost-chain-hl", "hovered chain highlight styled")
	must("#costs-table tr.cost-chain td:first-child", "chain rows carry the left accent")

	// Agent identity: the breakdown's first column leads with the row's
	// stable agent_id (shortAgentId — the rotation-immune `agt_` handle the
	// roster/audit/mail surfaces also lead with), falling back to the conv-id
	// prefix, and carries the full "<agent_id> / <conv-id>" pair on the id
	// span's hover title (idTooltip). The text filter matches the agent id
	// too, now that it leads the cell.
	must("shortAgentId(agent.agent_id, agent.conv_id)", "breakdown Agent cell leads with the stable agent id")
	must("idTooltip(agent.agent_id, agent.conv_id)", "full agent/conv id pair on the id cell tooltip")
	must("agent.agent_id", "text filter includes the agent id")
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
	must("monthProjection(narrowed, fillEmpty.value, includeWeekends.value", "projection receives the fill flag")
	must("function monthProjection(data, fillEmpty, weekendsIncluded", "projection takes the fill flag")

	// costs.js: the projection builds the leading-weekday fill and the
	// chart renders those columns as projected bars; the summary label
	// switches to the average-month wording when on.
	must("leadingFill", "projection computes the leading empty-weekday fill")
	must("Projected avg month total", "summary switches to the average-month label when filling")

	// costs.js: the toggle is inert (disabled) on the non-month spans
	// that have no projection.
	must("disabled=${current.span !== 'month'}", "toggle disabled off the month span")

	// dashboard.css: a disabled toggle is visibly dimmed.
	must(".filter-bar label.filter-toggle.disabled", "disabled toggle styled")
}

// TestDashboardHTML_CostsIncludeWeekendsWired guards the Costs tab's
// "include weekends" toggle across dashboard.html (the checkbox) and
// costs.js (the persisted flag, the per-day estimation basis it switches
// the month projection to, and the summary's unit wording). The toggle
// flips the projection from per-elapsed-weekday (weekends projected at
// zero) to per-elapsed-day (weekends projected at the per-day average) in
// the future projection and the leading fill alike. Pieces span two files
// with no JS test runner, so this asserts on the embedded concatenation
// at `go test ./...`.
func TestDashboardHTML_CostsIncludeWeekendsWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// dashboard.html: the checkbox + its label live in the span bar,
	// next to the fill-empty-weekdays toggle.
	must(`id="costs-include-weekends"`, "include-weekends checkbox present")
	must(`id="costs-include-weekends-label"`, "checkbox label present (toggled disabled off the month span)")
	must("include weekends", "checkbox label text present")

	// costs.js: persisted server-side via dashPrefs under its own key and
	// passed into the projection alongside the fill flag.
	must("tclaude.dash.costs.includeWeekends", "toggle persisted under its own pref key")
	must("monthProjection(narrowed, fillEmpty.value, includeWeekends.value", "projection receives the include-weekends flag")

	// costs.js: the single weekend switch — every day counts when on,
	// weekdays only otherwise — drives the denominator, the future
	// projection and the leading fill consistently.
	must("weekendsIncluded || !isWeekendKey(key)", "weekend switch generalises the estimation unit")
	must("weekendsIncluded", "projection reports whether weekends were included")

	// costs.js: the summary's per-unit figure switches between /weekday
	// and /day with the flag.
	must("projection?.weekendsIncluded ? 'day' : 'weekday'", "summary unit label tracks the flag")
}

// TestDashboardHTML_CostsMonthStepperWired guards the Costs tab's month
// browser across dashboard.html (the ‹ label › stepper), costs.js (the
// 'calmonth' span, the from/to range, the offset stepping and bounds) and
// dashboard.css (the stepper styling). The stepper is one continuous
// browser from the current month (offset 0, folded back into the 'month'
// span so it keeps its projection and lights the "This month" button
// alongside the stepper) back through completed months to the first month
// with recorded spend. Pieces span three files with no JS test runner, so
// this asserts on the embedded concatenation at `go test ./...`.
func TestDashboardHTML_CostsMonthStepperWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// dashboard.html: the stepper group with its prev / month / next
	// buttons, the month button carrying the dynamic 'calmonth' span.
	must(`id="costs-month-nav"`, "month stepper group present")
	must(`id="costs-month-prev"`, "older-month arrow present")
	must(`id="costs-month-cur"`, "month-label button present")
	must(`id="costs-month-next"`, "newer-month arrow present")
	must(`data-span="calmonth"`, "completed-month span carried on the label button")

	// costs.js: the completed-month range (from = first of month,
	// to = last of month) is sent as an explicit &to= bound so the server
	// caps the upper edge instead of running to today.
	must("function spanRange", "span range (from + to) computed")
	must("'calmonth'", "completed-month span key handled")
	must("&to=", "range's upper bound sent to /api/costs")

	// costs.js: entering the stepper, paging with the arrows, and the
	// bounds (› stops at the current month, ‹ stops at the first-data month).
	must("function activateMonth", "stepper activation implemented")
	must("current.monthOffset + 1", "older-month arrow stepping implemented")
	must("current.monthOffset >= current.oldestMonthOffset", "arrow enable/disable bounds implemented")
	must("oldestMonthOffset(data?.first_day", "‹ bound anchored at the first-ever costed month")
	must("MONTH_NAMES", "stepper label shows the month name")

	// costs.js: the current month (offset 0) is the head of the stepper —
	// "This month" routes through activateMonth(0), the span folds back into
	// 'month' there, and syncSpanHighlight lights the "This month" button
	// alongside the stepper so the two stay in sync and both read as active.
	must("monthView ? ' active'", "dual span highlight implemented")
	must("monthOffset.value = 0", "This month is the stepper's offset-0 head")

	// dashboard.css: the stepper group is styled (divider + tight arrows).
	must("#costs-month-nav", "stepper group styled")
	must("costs-month-step", "arrow buttons styled")
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
	must("async function loadFactor", "factor loaded when the tab opens")
	must("function saveFactor(raw)", "factor persisted + costs reloaded on edit")

	// config.js: round-trips cost.estimate_factor.
	must("estimate_factor", "config.js round-trips the cost.estimate_factor key")
}
