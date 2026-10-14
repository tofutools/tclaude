package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_DebugTabWired guards the bounded Debug (Activity ›
// Performance) Preact island's
// production wiring. Behaviour is covered by jstest/debug-preact.test.mjs.
func TestDashboardHTML_DebugTabWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// The Activity › Performance subtab + the panel activity-tab.js toggles
	// (the shared Activity wiring is pinned by TestDashboardHTML_ActivityTabWired).
	must(`data-subtab="performance" href="/activity/performance"`, "the Performance subtab")
	must(`id="activity-performance"`, "the Performance panel")
	must(`id="debug-root"`, "the one bounded Preact host")

	// State/actions own activation, monotonic request tokens, abortable I/O and
	// the slower active-only cadence.
	must("createDebugState", "Debug exposes Signals state")
	must("createDebugActions", "Debug exposes fetch/reset actions")
	must("function DebugApp", "Debug renders through a Preact component")
	must("/api/perf?limit=240", "Debug fetches the poll-timing endpoint")
	must("DEBUG_POLL_MS = 10_000", "Debug uses the slower debug-only poll cadence")
	must("if (!current.active)", "Debug gates work on the shared active-tab Signal")
	must("const timer = setIntervalImpl(actions.load, pollMs)", "Debug owns its active-only timer")
	must("clearIntervalImpl(timer)", "Debug cleans up its timer")
	must("request.controller.abort()", "Debug cancels in-flight requests")

	// The rendering pieces: sparkline, phase composition bar + legend,
	// and the aggregate table (the non-graphical encoding of the same
	// numbers — identity must never be color-alone).
	must("debug-spark", "the latency sparkline class")
	must("debug-phasebar", "the phase composition bar class")
	must("debug-legend", "the phase legend class")
	must("debug-table", "the per-phase aggregate table class")
	must("PHASE_COLORS", "the fixed phase color slots")
	must("key=${endpoint.endpoint}", "endpoint cards use stable Preact keys")
	must("key=${phase.name}", "phase rows and marks use stable Preact keys")

	// Alchemy is more than a nav-label swap in wizard mode: it gets a themed
	// heading, copy, cards, sparkline and table chrome. The categorical phase
	// fills intentionally retain PHASE_COLORS because they encode data.
	must(`class="debug-wizard-title"`, "the wizard-only Alchemy heading")
	must(`'⚗ clear readings'`, "the reset control's Alchemy copy")
	must(`class="debug-spark-area"`, "the sparkline area exposes a themeable class")
	must(`class="debug-spark-line"`, "the sparkline stroke exposes a themeable class")
	must("body.wizard #activity-performance .debug-card", "wizard mode themes the diagnostic cards")
	must("body.wizard #activity-performance .debug-spark-line", "wizard mode themes the latency sparkline")
	must("body.wizard #activity-performance .debug-table th", "wizard mode themes the phase table")

	// The loader claims the host and dashboard bootstrap mounts the feature.
	must("mountDebugFeature", "the loader exports and dashboard imports the feature")
	must("mountDebugFeature(),", "dashboard bootstrap mounts Debug with other islands")
	must("pageCleanups.push(...featureCleanups);", "page teardown invokes bounded feature cleanup")
	if strings.Contains(dashboardAssets, "bindDebugTab") {
		t.Error("dashboard assets still carry the retired Debug tab binder")
	}

	// The view is always shown: the retired dashboard.show_debug_tab opt-in
	// and its snapshot flag must not come back.
	for _, retired := range []string{"applyDebugTabVisibility", "debug_tab_visible", "body.hide-debug", `id="cfg-dashboard-show-debug-tab"`} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("dashboard assets still carry the retired Debug visibility gate %q", retired)
		}
	}

	// Reset remains a POST followed by a sequenced fresh GET.
	must(`id="debug-reset"`, "the reset-stats toolbar button")
	must("/api/perf/reset", "Debug posts the ring-clearing endpoint")
	must("async function reset()", "Debug exposes the reset action")
	must("return load();", "a successful reset immediately reloads the empty rings")
}
