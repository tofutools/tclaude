package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_AuditTabWired guards the Audit tab's wiring across
// dashboard.html + the Audit Preact feature graph + dashboard.js (JOH-268).
// This complements component tests by asserting on the embedded graph: a
// renamed mount, a dropped island, or a changed
// endpoint path surfaces here instead of as a blank tab at runtime.
func TestDashboardHTML_AuditTabWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// The nav button + the tab section the generic switcher toggles.
	must(`data-tab="audit"`, "the Audit nav button")
	must(`id="tab-audit"`, "the Audit tab section")
	must(`id="audit-root"`, "the stable Preact feature host")
	must(`id="audit-list"`, "the Preact-owned table mount")
	must(`id="filter-audit"`, "the server-side search input")
	must(`id="audit-outcome"`, "the outcome (success/failure) filter select")
	must(`id="audit-source"`, "the source (cli/dashboard) filter select")
	must(`id="audit-pager"`, "the pagination footer mount")

	// The action boundary fetches the endpoint and the component reacts to the
	// shared active-tab signal.
	must("/api/audit", "Audit actions fetch the audit read endpoint")
	must("current.active", "Audit island gates its lifecycle on tab activation")

	// Server-side search / sort / pagination wiring.
	must("page_size", "audit.js sends the page size to the server")
	must("audit-sort", "sortable column header class")
	must(`title="Next page"`, "the pager's next-page control")
	must("resetPage()", "a filter/sort change resets to page 1")

	// dashboard.js mounts the feature so the tab is live at boot.
	must("mountAuditFeature", "dashboard.js imports the feature loader")
	must("mountAuditFeature(),", "dashboard.js mounts the feature in the concurrent bounded group")

	// The symbolic rendering pieces: a verb chip, the operator chip, and
	// the status pill that distinguishes a denial from a success.
	must("audit-verb", "verb chip class")
	must("audit-actor", "actor chip class")
	must("statusView", "the outcome pill model")
}
