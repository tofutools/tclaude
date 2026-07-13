package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardDebugPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}

	state := read("js/debug-state.js")
	for _, needle := range []string{"activeTab.value === 'debug'", "acceptsRequest", "cancelRequest"} {
		if !strings.Contains(state, needle) {
			t.Errorf("Debug state missing %q", needle)
		}
	}
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Debug state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}

	actions := read("js/debug-actions.js")
	for _, needle := range []string{"/api/perf?limit=240", "/api/perf/reset", "request.controller.abort()", "return load();"} {
		if !strings.Contains(actions, needle) {
			t.Errorf("Debug actions missing %q", needle)
		}
	}
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "morphInto"} {
		if strings.Contains(actions, forbidden) {
			t.Errorf("Debug actions contain forbidden rendering knowledge %q", forbidden)
		}
	}

	island := read("js/debug-island.js")
	for _, needle := range []string{
		"function DebugApp", "DEBUG_POLL_MS = 10_000", "key=${endpoint.endpoint}",
		"key=${phase.name}", "clearIntervalImpl(timer)", "actions.dispose();",
	} {
		if !strings.Contains(island, needle) {
			t.Errorf("Debug island missing %q", needle)
		}
	}
	for _, forbidden := range []string{"innerHTML", "morphInto", "fetch(", "addEventListener", "./refresh.js"} {
		if strings.Contains(island, forbidden) {
			t.Errorf("Debug island bypasses its boundary with %q", forbidden)
		}
	}

	for _, needle := range []string{
		`<div id="debug-root"></div>`, "name: 'debug'", "state: debugState",
		"mountDebugFeature(),",
		"pageCleanups.push(...featureCleanups);",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Debug Preact wiring missing %q", needle)
		}
	}
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/debug.js"); err == nil {
		t.Error("retired legacy js/debug.js is still embedded")
	}
}
