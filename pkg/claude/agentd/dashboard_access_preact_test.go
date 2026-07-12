package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardAccessPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}
	state := read("js/access-state.js")
	for _, forbidden := range []string{"document", "querySelector", "innerHTML", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Access state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	island := read("js/access-island.js")
	for _, forbidden := range []string{"innerHTML", "morphInto", "fetch(", "./refresh.js"} {
		if strings.Contains(island, forbidden) {
			t.Errorf("Access island bypasses its boundary with %q", forbidden)
		}
	}
	for _, needle := range []string{`<div id="access-root"></div>`, "mountAccessFeature({", "name: 'access'", "state: accessState", "data-sudo-countdown=${grant.id}"} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Access Preact wiring missing %q", needle)
		}
	}
	for _, retired := range []string{"js/access-tab.js", "renderSudoTab", "renderPermissions", "renderSlugs", "bindAccessSubtabs", `data-act="sudo-revoke"`} {
		if strings.Contains(dashboardAssets, retired) {
			t.Errorf("Access migration left retired path %q", retired)
		}
	}
}
