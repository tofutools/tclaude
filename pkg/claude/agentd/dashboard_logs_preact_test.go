package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardLogsPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}
	state := read("js/logs-state.js")
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Logs state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	island := read("js/logs-island.js")
	for _, forbidden := range []string{"innerHTML", "morphInto", "fetch(", "./refresh.js"} {
		if strings.Contains(island, forbidden) {
			t.Errorf("Logs island bypasses its boundary with %q", forbidden)
		}
	}
	for _, needle := range []string{`<div id="logs-root"></div>`, "await mountLogsFeature();", "name: 'logs'", "state: logsState", "key=${key}"} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Logs Preact wiring missing %q", needle)
		}
	}
	for _, retired := range []string{"js/logs.js", "bindLogsTab();", "function renderLogs(", "morphInto($('#logs-list')"} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("Logs migration left retired path %q", retired)
		}
	}
}
