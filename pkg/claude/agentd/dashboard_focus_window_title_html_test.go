package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_FocusWindowTitle pins the Config-tab wiring for the
// focus.window_title toggle — the checkbox that lets a user turn off the
// tclaude:<id> window/tab title (ugly on a plain desktop terminal) at the
// cost of title-based window focus/tiling. Pure string contract: these
// literals must survive into the embedded dashboard assets, and they must
// match config.js's populate/assemble idiom exactly (default-true *bool:
// checked unless an explicit false, and only the non-default is stored).
func TestDashboardHTML_FocusWindowTitle(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	must(`id="cfg-focus-window-title"`, "the Config-tab checkbox ships")
	must("focus.window_title", "the hint names the stored config key")
	must("cfg.focus && cfg.focus.window_title === false", "config.js populates the checkbox (checked unless explicit false)")
	must("fc.window_title = false", "config.js assembles only the non-default on save")
}
