package agentd

import (
	"errors"
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
	actions := read("js/links-actions.js")
	controller := read("js/links-controller.js")
	loader := read("js/preact-loader.js")
	dashboard := read("js/dashboard.js")
	html := read("dashboard.html")
	tabs := read("js/tabs.js")
	rowActions := read("js/row-action-handler.js")
	if _, err := fs.Stat(dashboardAssetsFS, "js/modal-link-wt.js"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("retired modal-link-wt.js must stay absent, stat error: %v", err)
	}

	for _, needle := range []string{
		"export function LinksControls(",
		"export function LinksList(",
		"function LinksManager(",
		"function LinkEditor(",
		`id="links-manage-modal"`,
		`id="link-modal"`,
		"key=${String(link.id)}",
		"onClick=${() => actions.openCreate()}",
		"onClick=${() => actions.openEdit(",
		"onClick=${() => remove(link)}",
		"onClick=${() => state.cycleSort(column.col)}",
		"registerLinksController(actions)",
	} {
		if !strings.Contains(island, needle) {
			t.Errorf("links-island.js missing %q", needle)
		}
	}
	for _, forbidden := range []string{"innerHTML", "morphInto", "fetch(", "./refresh.js", `data-act="link-edit"`, `data-act="link-delete"`} {
		if strings.Contains(island+state, forbidden) {
			t.Errorf("Links bounded owner contains forbidden legacy dependency %q", forbidden)
		}
	}
	for _, needle := range []string{
		"export function createLinksState(",
		"computed(() =>",
		"persistTableSort",
		"prefs.setItem(FILTER_KEY, next)",
		"managerOpen = signal(false)",
		"editor = signal(null)",
		"if (editor.value) return false",
	} {
		if !strings.Contains(state, needle) {
			t.Errorf("links-state.js missing %q", needle)
		}
	}
	for _, needle := range []string{
		"const linksDescriptor = createIslandDescriptor({",
		"host: '#links-feature-root'",
		"mountLinksFeature",
		"createLinksActions",
		"confirmDiscard: dependencies.confirmDiscard",
	} {
		if !strings.Contains(loader, needle) {
			t.Errorf("preact-loader.js missing Links ownership contract %q", needle)
		}
	}
	if !strings.Contains(dashboard, "mountLinksFeature({") || !strings.Contains(dashboard, "words: wizWord") {
		t.Error("dashboard bootstrap does not mount the Links island")
	}
	for _, needle := range []string{
		"export function createLinksActions(",
		"async createLink(",
		"async updateLink(",
		"async deleteLink(",
		"await refresh()",
	} {
		if !strings.Contains(actions, needle) {
			t.Errorf("links-actions.js missing plain mutation boundary %q", needle)
		}
	}
	for _, forbidden := range []string{"document.", "querySelector", "innerHTML"} {
		if strings.Contains(actions, forbidden) {
			t.Errorf("plain Links actions retain DOM dependency %q", forbidden)
		}
	}
	for _, needle := range []string{
		"registerLinksController", "openLinksManager", "openLinkCreate", "openLinkEdit", "deleteLink",
	} {
		if !strings.Contains(controller, needle) {
			t.Errorf("links-controller.js missing delegated launcher seam %q", needle)
		}
	}
	if !strings.Contains(html, `<div id="links-feature-root"></div>`) {
		t.Error("dashboard does not expose the exclusive Links feature host")
	}
	for _, forbidden := range []string{`<div class="manage-overlay" id="links-manage-modal">`, `<div class="modal-overlay" id="link-modal">`} {
		if strings.Contains(html, forbidden) {
			t.Errorf("static Links dialog markup remains: %q", forbidden)
		}
	}
	if !strings.Contains(tabs, "featureState('links')?.publish(lastSnapshot)") {
		t.Error("legacy Links adapter does not publish into the bounded island")
	}
	for _, forbidden := range []string{"bindFilter('links')", "bindSortHeaders", "renderLinks(", "bindLinkModal", "openLinkModal"} {
		if strings.Contains(dashboard+tabs, forbidden) {
			t.Errorf("legacy Links ownership remains: %q", forbidden)
		}
	}
	for _, needle := range []string{"openLinkCreate({ from })", "openLinksManager()", "openLinkEdit({ id, from, to, mode })", "await deleteLink({ id, from, to, scope })"} {
		if !strings.Contains(rowActions, needle) {
			t.Errorf("delegated group launcher does not use Links controller: %q", needle)
		}
	}
}
