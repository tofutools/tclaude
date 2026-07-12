package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_DebugTabWired guards the Debug tab's wiring across
// dashboard.html + debug.js + dashboard.js (TCL-376). The repo has no JS
// test runner, so this asserts on the embedded asset concatenation at
// `go test ./...`: a renamed mount, a dropped binder, or a changed
// endpoint path surfaces here instead of as a blank tab at runtime.
func TestDashboardHTML_DebugTabWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// The nav button + the tab section the generic switcher toggles.
	must(`data-tab="debug"`, "the Debug nav button")
	must(`id="tab-debug"`, "the Debug tab section")
	must(`id="debug-list"`, "the card mount debug.js renders into")
	must(`id="debug-updated"`, "the last-updated stamp mount")

	// debug.js fetches the perf endpoint and binds the tab.
	must("/api/perf", "debug.js fetches the poll-timing endpoint")
	must("function bindDebugTab", "debug.js exposes the tab binder")
	must(`nav [data-tab="debug"]`, "debug.js loads on tab activation")
	must("tclaude:snapshot", "debug.js re-fetches on the snapshot tick while active")

	// The rendering pieces: sparkline, phase composition bar + legend,
	// and the aggregate table (the non-graphical encoding of the same
	// numbers — identity must never be color-alone).
	must("debug-spark", "the latency sparkline class")
	must("debug-phasebar", "the phase composition bar class")
	must("debug-legend", "the phase legend class")
	must("debug-table", "the per-phase aggregate table class")
	must("PHASE_COLORS", "the fixed phase color slots")

	// dashboard.js imports + calls the binder so the tab is live at boot.
	must("import { bindDebugTab }", "dashboard.js imports the binder")
	must("bindDebugTab();", "dashboard.js calls the binder at boot")

	// The URL router treats /debug as a routable location on both sides
	// (client sets; the server-side allow-list is covered by
	// TestDashboardAppPaths_SPAFallback).
	must(`'debug', 'config', 'vegas',`, "nav-history-core.js KNOWN_TABS includes debug")
	must(`'debug', 'config',`, "nav-history.js ROUTABLE_TABS includes debug")

	// The visibility gate (config dashboard.show_debug_tab, default off):
	// refresh.js applies the snapshot flag, CSS hides the chrome, and the
	// Config tab carries the opt-in checkbox with its load/save wiring.
	must("applyDebugTabVisibility", "refresh.js applies the snapshot visibility flag")
	must("debug_tab_visible", "the snapshot flag the front-end keys off")
	must(`body.hide-debug nav [data-tab="debug"]`, "CSS hides the nav button when gated off")
	must(`id="cfg-dashboard-show-debug-tab"`, "the Config tab opt-in checkbox")
	must("show_debug_tab", "config.js round-trips dashboard.show_debug_tab")

	// The stats-reset control (TCL-377): the toolbar button and the POST
	// endpoint debug.js calls before re-fetching.
	must(`id="debug-reset"`, "the reset-stats toolbar button")
	must("/api/perf/reset", "debug.js posts the ring-clearing endpoint")
	must("function resetDebug", "debug.js exposes the reset handler")
}
