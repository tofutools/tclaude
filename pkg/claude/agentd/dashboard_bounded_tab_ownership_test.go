package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// Bounded migrations should retire feature-owned legacy code without editing
// the authoritative poll's internals. This pins the mechanical TCL-373 split
// so per-tab rendering does not drift back into refresh.js.
func TestDashboardBoundedTabOwnership(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}

	refresh := read("js/refresh.js")
	for _, forbidden := range []string{
		"function applyCostTabVisibility(",
		"function applyPluginsTabVisibility(",
		"renderPermissions(data.permissions",
		"renderSlugs(data.slugs",
		"renderPluginsTab();",
	} {
		if strings.Contains(refresh, forbidden) {
			t.Errorf("refresh.js regained bounded-tab implementation %q", forbidden)
		}
	}

	want := map[string][]string{
		"js/costs.js": {
			"function applyCostTabVisibility(",
			"dashboardState.setActiveTab('groups')",
		},
		"js/plugins.js": {
			"export function renderPluginsSnapshot(data)",
			"applyPluginsTabVisibility(data)",
			"bindFilter('plugins', renderPluginsTab)",
		},
		"js/access-tab.js": {
			"export function renderAccessSnapshot(data)",
			"morphInto($('#permissions-body'), renderPermissions(",
			"morphInto($('#slugs-body'), renderSlugs(",
		},
	}
	for name, needles := range want {
		contents := read(name)
		for _, needle := range needles {
			if !strings.Contains(contents, needle) {
				t.Errorf("%s missing feature-owned wiring %q", name, needle)
			}
		}
	}
}
