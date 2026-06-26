package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_PluginsTab guards the Plugins tab's wiring across
// the embedded assets: the nav button (with its warning badge), the
// tab section + filter bar, the create/edit modal, and the JS module
// hooks the Go snapshot fields feed. Mirrors the Agents-tab guard:
// string-level checks over the concatenated embedded sources, so a
// refactor can't silently drop one side of an id contract.
func TestDashboardHTML_PluginsTab(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// Nav button + warning badge.
	present(`data-tab="plugins"`, "the Plugins nav button")
	present(`id="plugins-badge"`, "the warning badge on the nav button")
	present(`class="tab-badge warn"`, "the badge uses the amber warn variant")

	// Tab section + filter bar + toolbar buttons.
	present(`id="tab-plugins"`, "the Plugins tab section")
	present(`id="filter-plugins"`, "the Plugins filter input")
	present(`id="plugins-check-now"`, "the check-all-now button")
	present(`id="plugin-create-open"`, "the new-plugin button")
	present(`id="plugins-list"`, "the listing container")

	// Create/edit modal.
	present(`id="plugin-modal"`, "the plugin modal overlay")
	present(`id="plugin-modal-steps"`, "the dynamic step-rows container")
	present(`id="plugin-modal-add-step"`, "the add-step button")
	present(`id="plugin-modal-submit"`, "the modal submit button")

	// JS renderer + snapshot contract. The snapshot carries plugins /
	// plugins_catalog / plugins_warn (dashboard.go); the module must
	// read the same keys.
	present("function renderPluginsTab(", "the tab renderer in plugins.js")
	present("function renderPluginsBadge(", "the badge renderer in plugins.js")
	present("lastSnapshot.plugins_catalog", "renderer reads the catalog snapshot field")
	present("data.plugins_warn", "refresh feeds the badge from the snapshot")
	present(`data-act="plugin-step-toggle"`, "the per-step run/stop lamp")
	present(`data-act="plugin-toggle"`, "the whole-plugin activate/deactivate lamp")
	present(`data-step-stop`, "the modal's per-step stop command field")
	present(`data-act="plugin-install"`, "the catalog install button")
}

// TestDashboardHTML_PluginsTabAutoHide guards the Plugins-tab auto-hide
// wiring across the embedded assets, mirroring the Costs-tab auto-hide
// guard. The server's plugins_tab_visible flag (dashboard.go) drives a
// body.hide-plugins CSS class via applyPluginsTabVisibility in refresh.js;
// the Config tab exposes the dashboard.always_show_plugins_tab opt-in. A
// rename in any one file silently re-shows (or strands) the tab, so pin all
// three sides together.
func TestDashboardHTML_PluginsTabAutoHide(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// CSS: both the nav button and the section hide on body.hide-plugins so
	// the tab vanishes entirely.
	present(`body.hide-plugins nav button[data-tab="plugins"]`, "the Plugins nav button hides on body.hide-plugins")
	present("body.hide-plugins #tab-plugins", "the Plugins section hides alongside its nav button")

	// JS: refresh.js reads the server's flag and toggles the body class.
	present("function applyPluginsTabVisibility(", "the visibility applier in refresh.js")
	present("data.plugins_tab_visible", "visibility reads the server's plugins_tab_visible flag")
	present("'hide-plugins'", "refresh toggles body.hide-plugins")

	// Config tab: the always-show opt-in checkbox, loaded + saved by config.js.
	present(`id="cfg-dashboard-always-show-plugins"`, "the Config-tab always-show-plugins checkbox")
	present("dashboard.always_show_plugins_tab", "config.js reads/writes the opt-in key")
}
