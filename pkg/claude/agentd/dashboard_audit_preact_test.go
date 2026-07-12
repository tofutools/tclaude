package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardAuditPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}
	state := read("js/audit-state.js")
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Audit state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	island := read("js/audit-island.js")
	for _, forbidden := range []string{"innerHTML", "morphInto", "fetch(", "./refresh.js"} {
		if strings.Contains(island, forbidden) {
			t.Errorf("Audit island bypasses its boundary with %q", forbidden)
		}
	}
	for _, needle := range []string{`<div id="audit-root"></div>`, "mountAuditFeature(),", "name: 'audit'", "state: auditState", "key=${entry.id}", "tclaude:tab-reselected"} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Audit Preact wiring missing %q", needle)
		}
	}
	for _, retired := range []string{"js/audit.js", "bindAuditTab();", "function renderAudit(", "morphInto($('#audit-list')"} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("Audit migration left retired path %q", retired)
		}
	}
}
