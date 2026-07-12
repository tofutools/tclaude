package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_LogsTabWired guards the Logs tab's wiring across
// dashboard.html + the Logs Preact feature graph + dashboard.js. This
// complements component tests by asserting on the embedded asset graph: a
// renamed mount, a dropped island, or a changed
// endpoint path surfaces here instead of as a blank tab at runtime.
func TestDashboardHTML_LogsTabWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// The nav button + the tab section the generic switcher toggles.
	must(`data-tab="logs"`, "the Logs nav button")
	must(`id="tab-logs"`, "the Logs tab section")
	must(`id="logs-root"`, "the stable Preact feature host")
	must(`id="logs-list"`, "the Preact-owned table mount")
	must(`id="filter-logs"`, "the server-side search input")
	must(`id="logs-level"`, "the minimum-level filter select")
	must(`id="logs-range"`, "the time-range (since) filter select")
	must(`id="logs-rotated"`, "the include-rotated-files toggle")
	must(`id="logs-hide-raw"`, "the hide-raw (non-JSON) toggle")
	must(`id="logs-refresh"`, "the manual refresh button")
	must(`id="logs-stream"`, "the streaming (tail-follow) toggle")
	must(`id="logs-pager"`, "the pagination footer mount")

	// The action boundary fetches the read endpoint and the component reacts to
	// the shared active-tab signal.
	must("/api/logs", "Logs actions fetch the logs read endpoint")
	must("current.active", "Logs island gates its lifecycle on tab activation")

	// Server-side search / filter / pagination wiring.
	must("page_size", "logs.js sends the page size to the server")
	must("include_rotated", "logs.js sends the rotated-files flag")
	must("hide_raw", "logs.js sends the hide-raw flag")
	must(`title="Next page (older)"`, "the pager's next-page control")
	must("resetPage()", "a filter change resets to page 1")
	must("STREAM_MS", "logs.js has a tail-follow poll interval")

	// dashboard.js mounts the feature so the tab is live at boot.
	must("mountLogsFeature", "dashboard.js imports the feature loader")
	must("mountLogsFeature(),", "dashboard.js mounts the feature in the concurrent bounded group")

	// The level-pill rendering (colourises debug/info/warn/error + raw).
	must("log-level", "the level pill class")
	must("levelKey(row.level)", "the level pill model mapping")

	// The source-reporting strip: which files were read (with per-file
	// counts) and the click-to-include-rotated hint.
	must("data.sources", "Logs island reads the per-file sources list")
	must("rotated_available", "logs.js reads the on-disk rotated-file count")
	must("logs-sources", "the sources summary span (+ its tooltip breakdown)")
	must("logs-rotated-hint", "the click-to-include-rotated hint")
}
