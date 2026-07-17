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
	for _, needle := range []string{"<polyline", "usage-observed-line", "usage-forecast-line", "usage-reset-mark"} {
		if !strings.Contains(chart, needle) {
			t.Errorf("Usage line chart missing %q", needle)
		}
	}
	for _, needle := range []string{
		`data-tab="usage"`, `<div id="usage-root"></div>`, "mountUsageHistoryFeature(),",
		"name: 'usage'", "/api/usage-history?hours=", "Forecasts are per provider × quota window",
		"body.hide-usage-tab nav [data-tab=\"usage\"]",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Usage Preact wiring missing %q", needle)
		}
	}
}
