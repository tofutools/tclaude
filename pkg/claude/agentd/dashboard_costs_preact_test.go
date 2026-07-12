package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardCostsPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}

	state := read("js/costs-state.js")
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Costs state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	island := read("js/costs-island.js")
	for _, forbidden := range []string{"innerHTML", "morphInto", "fetch(", "./refresh.js"} {
		if strings.Contains(island, forbidden) {
			t.Errorf("Costs island bypasses its component/action boundary with %q", forbidden)
		}
	}
	chart := read("js/costs-chart.js")
	for _, needle := range []string{
		"export function mountImperativeCostChart(",
		"host.addEventListener('mousemove', move)",
		"host.removeEventListener('mousemove', move)",
		"tooltip?.remove()",
		"useEffect(() => mountImperativeCostChart(host.current, chart), [chart])",
	} {
		if !strings.Contains(chart, needle) {
			t.Errorf("imperative chart boundary missing lifecycle wiring %q", needle)
		}
	}

	for _, needle := range []string{
		`<div id="costs-root"></div>`,
		"await mountCostsFeature();",
		"return mountFeatureIsland({",
		"name: 'costs'",
		"state: costsState",
		"key=${`${agent.conv_id}:${agent.day}`}",
		"function saveFactor(raw)",
		"if (!state.commitFactor(token",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Costs Preact wiring missing %q", needle)
		}
	}

	for _, retired := range []string{
		"js/costs.js", "bindCostsTab();", "applyCostTabVisibility(data);",
		"morphInto($('#costs-table'),", "morphInto($('#costs-summary'),",
		"function bindCostsChartTip(", "function bindCostsSort(",
	} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("Costs migration left retired legacy path %q", retired)
		}
	}
}
