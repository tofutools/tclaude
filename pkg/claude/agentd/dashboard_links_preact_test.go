package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardLinksPreactOwnership(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(body)
	}

	island := read("js/links-island.js")
	state := read("js/links-state.js")
	loader := read("js/preact-loader.js")
	dashboard := read("js/dashboard.js")
	tabs := read("js/tabs.js")

	for _, needle := range []string{
		"export function LinksControls(",
		"export function LinksList(",
		"key=${String(link.id)}",
		`data-act="link-edit"`,
		`data-act="link-delete"`,
		"onClick=${() => state.cycleSort(column.col)}",
	} {
		if !strings.Contains(island, needle) {
			t.Errorf("links-island.js missing %q", needle)
		}
	}
	for _, forbidden := range []string{"innerHTML", "morphInto", "fetch(", "./refresh.js"} {
		if strings.Contains(island+state, forbidden) {
			t.Errorf("Links bounded owner contains forbidden legacy dependency %q", forbidden)
		}
	}
	for _, needle := range []string{
		"export function createLinksState(",
		"computed(() =>",
		"persistTableSort",
		"prefs.setItem(FILTER_KEY, next)",
	} {
		if !strings.Contains(state, needle) {
			t.Errorf("links-state.js missing %q", needle)
		}
	}
	for _, needle := range []string{
		"const linksDescriptor = createIslandDescriptor({",
		"filterHost: '#links-filter-root'",
		"listHost: '#links-list'",
		"mountLinksFeature",
	} {
		if !strings.Contains(loader, needle) {
			t.Errorf("preact-loader.js missing Links ownership contract %q", needle)
		}
	}
	if !strings.Contains(dashboard, "mountLinksFeature(),") {
		t.Error("dashboard bootstrap does not mount the Links island")
	}
	if !strings.Contains(tabs, "featureState('links')?.publish(lastSnapshot)") {
		t.Error("legacy Links adapter does not publish into the bounded island")
	}
	for _, forbidden := range []string{"bindFilter('links')", "bindSortHeaders", "renderLinks("} {
		if strings.Contains(dashboard+tabs, forbidden) {
			t.Errorf("legacy Links ownership remains: %q", forbidden)
		}
	}
}
