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
		"renderLinksTab();",
		"applyProcessesTabVisibility(data);",
		"applyDebugTabVisibility(data);",
		"renderMailTab();",
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

	want := map[string][]string{"js/access-island.js": {"export function mountAccessIsland(", "function PermissionsView(", "function SlugsView(", "function SudoView("}}
	for name, needles := range want {
		contents := read(name)
		for _, needle := range needles {
			if !strings.Contains(contents, needle) {
				t.Errorf("%s missing feature-owned wiring %q", name, needle)
			}
		}
	}

	dashboard := read("js/dashboard.js")
	if got := strings.Count(dashboard, "mountAccessFeature({"); got != 1 {
		t.Errorf("Access island mount count = %d, want 1", got)
	}
	jobsMount := strings.Index(dashboard, "await mountJobsFeature({")
	concurrentMounts := strings.Index(dashboard, "await Promise.all([")
	bindTabs := strings.Index(dashboard, "bindTabs();")
	if jobsMount < 0 || concurrentMounts < jobsMount || bindTabs < concurrentMounts {
		t.Error("dashboard boot must await Jobs, then bounded islands concurrently, before binding navigation")
	}
	if strings.Contains(dashboard, "bindFilter('sudo')") || strings.Contains(dashboard, "bindFilter('plugins')") {
		t.Error("dashboard bootstrap retains duplicate bounded-tab filter binding")
	}
	for _, retired := range []string{"./plugins.js", "renderPluginsSnapshot", "bindPluginsUI"} {
		if strings.Contains(refresh+dashboard, retired) {
			t.Errorf("legacy Plugins ownership remains in core graph: %q", retired)
		}
	}
	for _, retired := range []string{"./costs.js", "applyCostTabVisibility", "bindCostsTab"} {
		if strings.Contains(refresh+dashboard, retired) {
			t.Errorf("legacy Costs ownership remains in core graph: %q", retired)
		}
	}
	for _, retired := range []string{"./access-tab.js", "renderAccessListSnapshot", "renderAccessRegistrySnapshot", "bindAccessSubtabs"} {
		if strings.Contains(refresh+dashboard, retired) {
			t.Errorf("legacy Access ownership remains in core graph: %q", retired)
		}
	}
}
