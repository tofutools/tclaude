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
		// Centring must ride in each element's own margin shorthand. A shared
		// `margin-inline: auto` rule is silently reset by any later shorthand
		// on the same element -- .filter-bar does that to the controls bar,
		// hence the two-class selector here.
		".filter-bar.usage-history-controls { max-width: var(--usage-card-max); margin: 0 auto 12px; }",
		"margin: 0 auto 14px",
		"margin: -4px auto 12px",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Usage Preact wiring missing %q", needle)
		}
	}
	if strings.Contains(chart, "point.source") {
		t.Error("Usage point tooltip exposes internal sample source")
	}
}
