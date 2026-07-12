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
	ordered := []string{
		"renderDock();",
		"renderAccessListSnapshot();",
		"renderLinksTab();",
		"renderPluginsSnapshot(data);",
		"applyProcessesTabVisibility(data);",
		"applyDebugTabVisibility(data);",
		"renderAccessRegistrySnapshot(data);",
		"renderMailTab();",
		"applyCostTabVisibility(data);",
		"dashboardState.commitRequest(requestId, data);",
	}
	last := -1
	for _, needle := range ordered {
		at := strings.Index(refresh[last+1:], needle)
		if at < 0 {
			t.Errorf("refresh.js missing bounded-tab orchestration %q", needle)
			continue
		}
		last += at + 1
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
			"export function renderAccessListSnapshot()",
			"export function renderAccessRegistrySnapshot(data)",
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

	access := read("js/access-tab.js")
	plugins := read("js/plugins.js")
	dashboard := read("js/dashboard.js")
	if got := strings.Count(access, "bindFilter('sudo', renderSudoTab)"); got != 1 {
		t.Errorf("Access sudo filter binding count = %d, want 1", got)
	}
	if got := strings.Count(plugins, "bindFilter('plugins', renderPluginsTab)"); got != 1 {
		t.Errorf("Plugins filter binding count = %d, want 1", got)
	}
	if got := strings.Count(dashboard, "bindAccessTab();"); got != 1 {
		t.Errorf("Access bootstrap binding count = %d, want 1", got)
	}
	if strings.Contains(dashboard, "bindFilter('sudo')") || strings.Contains(dashboard, "bindFilter('plugins')") {
		t.Error("dashboard bootstrap retains duplicate bounded-tab filter binding")
	}
}
