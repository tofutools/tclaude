package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_AuditTabWired guards the Audit tab's wiring across
// dashboard.html + audit.js + dashboard.js. The repo has no JS test
// runner, so this asserts on the embedded asset concatenation at
// `go test ./...`: a renamed mount, a dropped binder, or a changed
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
	must(`id="audit-list"`, "the table mount audit.js renders into")
	must(`id="filter-audit"`, "the client-side text filter input")
	must(`id="audit-outcome"`, "the outcome (success/failure) filter select")
	must(`id="audit-source"`, "the source (cli/dashboard) filter select")

	// audit.js fetches the read endpoint and binds the tab.
	must("/api/audit", "audit.js fetches the audit read endpoint")
	must("function bindAuditTab", "audit.js exposes the tab binder")
	must(`nav button[data-tab="audit"]`, "audit.js loads on tab activation")

	// dashboard.js imports + calls the binder so the tab is live at boot.
	must("import { bindAuditTab }", "dashboard.js imports the binder")
	must("bindAuditTab();", "dashboard.js calls the binder at boot")

	// The symbolic rendering pieces: a verb chip, the operator chip, and
	// the status pill that distinguishes a denial from a success.
	must("audit-verb", "verb chip class")
	must("audit-actor", "actor chip class")
	must("statusPill", "the outcome pill builder")
}
