package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardUsageHistoryPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}
	state := read("js/usage-history-state.js")
	for _, forbidden := range []string{"document", "querySelector", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Usage state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	chart := read("js/usage-history-chart.js")
	for _, needle := range []string{
		"<polyline", "usage-observed-line", "usage-forecast-line", "usage-reset-mark",
		"usageAxisTicks(start, horizon)", "usage-forecast-hit-target", "usage-scheduled-reset",
		"usage-marker-hit-target", "usage-chart-tooltip",
	} {
		if !strings.Contains(chart, needle) {
			t.Errorf("Usage line chart missing %q", needle)
		}
	}
	if strings.Contains(chart, "previous.pct - point.pct") {
		t.Error("Usage chart infers reset boundaries after downsampling instead of using server markers")
	}
	for _, needle := range []string{
		`data-tab="usage"`, `<div id="usage-root"></div>`, "mountUsageHistoryFeature(),",
		"name: 'usage'", "/api/usage-history?hours=", "Forecasts are per provider × quota window",
		"body.hide-usage-tab nav [data-tab=\"usage\"]",
		"if (name === 'seven_day_sonnet') return '7 day Sonnet';",
		"sampledPoints(points, 240)",
		"headline: 'Prediction paused'",
		"Usage chart legend",
		"USAGE_LOOKAHEAD_SPANS",
		"Look ahead",
		"`History range, ${scope}`",
		"`Forecast lookahead, ${scope}`",
		"aria-pressed=",
		"usage-card-controls",
		"&spans=",
		"tclaude.dash.usage.seriesSpans",
		// A provider's quota windows share one centred row, capped at the
		// per-card width times that row's card count.
		"groupSeriesByProvider(current.series)",
		"usage-provider-row",
		"--usage-cols:${row.series.length}",
		// The row-width variables live on the host, so the island-load-failure
		// banner (rendered into #usage-root, outside the island) inherits them.
		"#usage-root { --usage-card-max: 1100px; --usage-card-gap: 14px; }",
		// Centring must ride in each element's own margin shorthand: a shared
		// `margin-inline: auto` rule is silently reset by any later shorthand
		// on the same element.
		"margin-inline: auto; width: 100%;",
		// The legend renders per card, under its chart, with a scoped label —
		// side-by-side graphs have no line of sight to one shared legend.
		"function UsageChartLegend({ scope })",
		"`Usage chart legend, ${scope}`",
		"<${UsageChartLegend} scope=${scope} />",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Usage Preact wiring missing %q", needle)
		}
	}
	if strings.Contains(chart, "point.source") {
		t.Error("Usage point tooltip exposes internal sample source")
	}
	// The tab must open on the graphs. Nothing explanatory sits above the grid:
	// the old top-of-tab controls bar is gone entirely, and the note is a
	// footnote after the grid rather than a header before it.
	island := read("js/usage-history-island.js")
	if strings.Contains(island, "usage-history-controls") {
		t.Error("Usage tab reintroduced the top-of-tab controls bar above the graphs")
	}
	if strings.Index(island, "usage-history-note") < strings.Index(island, "usage-series-grid") {
		t.Error("Usage explanatory note renders above the graph grid instead of below it")
	}
	// The note is a full-width footnote: capping or centring it would line it
	// up with the rows above, which is what moving it out of the header undid.
	if noteRule := usageCSSRule(t, read("dashboard.css"), ".usage-history-note"); strings.Contains(noteRule, "max-width") ||
		strings.Contains(noteRule, "auto") {
		t.Errorf("Usage note should be uncapped and left-aligned, got rule: %s", noteRule)
	}
}

// usageCSSRule returns the declaration block of the first rule whose selector
// list is exactly `selector`, for asserting on one rule rather than the whole
// stylesheet.
func usageCSSRule(t *testing.T, css, selector string) string {
	t.Helper()
	start := strings.Index(css, "\n"+selector+" {")
	if start < 0 {
		t.Fatalf("no %q rule in dashboard.css", selector)
	}
	body := css[start+len("\n"+selector+" {"):]
	end := strings.Index(body, "}")
	if end < 0 {
		t.Fatalf("unterminated %q rule in dashboard.css", selector)
	}
	return body[:end]
}
