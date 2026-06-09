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
	present(`data-act="plugin-run-step"`, "the per-step run button")
	present(`data-act="plugin-install"`, "the catalog install button")
}
