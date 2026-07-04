package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_LogsTabWired guards the Logs tab's wiring across
// dashboard.html + logs.js + dashboard.js. The repo has no JS test
// runner, so this asserts on the embedded asset concatenation at
// `go test ./...`: a renamed mount, a dropped binder, or a changed
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
	must(`id="logs-list"`, "the table mount logs.js renders into")
	must(`id="filter-logs"`, "the server-side search input")
	must(`id="logs-level"`, "the minimum-level filter select")
	must(`id="logs-range"`, "the time-range (since) filter select")
	must(`id="logs-rotated"`, "the include-rotated-files toggle")
	must(`id="logs-hide-raw"`, "the hide-raw (non-JSON) toggle")
	must(`id="logs-refresh"`, "the manual refresh button")
	must(`id="logs-stream"`, "the streaming (tail-follow) toggle")
	must(`id="logs-pager"`, "the pagination footer mount")

	// logs.js fetches the read endpoint and binds the tab.
	must("/api/logs", "logs.js fetches the logs read endpoint")
	must("function bindLogsTab", "logs.js exposes the tab binder")
	must(`nav button[data-tab="logs"]`, "logs.js loads on tab activation")

	// Server-side search / filter / pagination wiring.
	must("page_size", "logs.js sends the page size to the server")
	must("include_rotated", "logs.js sends the rotated-files flag")
	must("hide_raw", "logs.js sends the hide-raw flag")
	must("logs-page-next", "the pager's next-page control")
	must("reloadFromFirstPage", "a filter change resets to page 1")
	must("STREAM_MS", "logs.js has a tail-follow poll interval")

	// dashboard.js imports + calls the binder so the tab is live at boot.
	must("import { bindLogsTab }", "dashboard.js imports the binder")
	must("bindLogsTab();", "dashboard.js calls the binder at boot")

	// The level-pill rendering (colourises debug/info/warn/error + raw).
	must("log-level", "the level pill class")
	must("levelPill", "the level pill builder")

	// The source-reporting strip: which files were read (with per-file
	// counts) and the click-to-include-rotated hint.
	must("data.sources", "logs.js reads the per-file sources list")
	must("rotated_available", "logs.js reads the on-disk rotated-file count")
	must("logs-sources", "the sources summary span (+ its tooltip breakdown)")
	must("logs-rotated-hint", "the click-to-include-rotated hint")
}
