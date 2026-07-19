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
		"--usage-card-max",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Usage Preact wiring missing %q", needle)
		}
	}
	if strings.Contains(chart, "point.source") {
		t.Error("Usage point tooltip exposes internal sample source")
	}
}
