package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_ConfigSectionFilter guards the Config-tab section
// filter across the files it spans: dashboard.html hosts the search bar,
// config.js implements the live show/hide, dashboard.css styles the
// no-match line. The repo has no JS test runner, so this asserts on the
// embedded source concatenation at `go test ./...`.
//
// The load-bearing invariant is that the filter resolves its sections
// LIVE from the DOM (every .cfg-section), so a section added to
// dashboard.html is picked up automatically — there must be NO hardcoded
// list of section names anywhere. The selector assertion is how we lock
// that in.
func TestDashboardHTML_ConfigSectionFilter(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// dashboard.html: the search box, its count/clear chrome, and the
	// no-match line the JS toggles.
	must(`id="cfg-filter"`, "the Config-tab filter search box exists")
	must(`id="cfg-filter-count"`, "the filter match-count element exists")
	must(`id="cfg-filter-clear"`, "the filter clear button exists")
	must(`id="cfg-filter-empty"`, "the no-match line exists")

	// config.js: the filter is implemented and wired.
	must("function applyConfigFilter(", "the filter apply fn is defined")
	must("function cfgFilterBlocks(", "the live section resolver is defined")
	must("function cfgSearchText(", "the title+content haystack builder is defined")
	must("addEventListener('input', applyConfigFilter)", "the search box drives the filter")

	// The auto-pickup guarantee: sections are resolved live by class, not
	// from a hardcoded name list. This selector is the contract — a new
	// .cfg-section is filtered with no JS change.
	must("'#tab-config .cfg-section, #tab-config > details.cfg-advanced'",
		"sections are resolved live from the DOM (no hardcoded section list)")

	// A standing query survives a Reload (loadConfigTab rebuilds inner lists).
	must("applyConfigFilter();", "the filter is re-applied after a reload")
}
