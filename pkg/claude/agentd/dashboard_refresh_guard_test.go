package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's auto-refresh used to wedge permanently after a
// drag-and-drop retire because legacy rendering could detach the dragged row
// before dragend reset its global suspension flag. Preact now owns keyed group
// rows, menus, modals, slop machines, and the dashboard profile picker. No UI
// draft is allowed to suspend the poll or become DOM-backed request state.
//
// The fix lives entirely in dashboard.html's embedded JS, so there is
// no server code path a flow test can exercise. This guards the shape
// of that fix against a future refactor of the file silently undoing
// it. See the PR for the manual browser repro.
func TestDashboardHTML_RefreshGuardCannotWedge(t *testing.T) {
	refreshBytes, err := fs.ReadFile(dashboardAssetsFS, "js/refresh.js")
	if err != nil {
		t.Fatalf("read refresh.js: %v", err)
	}
	refreshSource := string(refreshBytes)
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html still contains %q (%s)", needle, why)
		}
	}
	mustNotRefresh := func(needle, why string) {
		t.Helper()
		if strings.Contains(refreshSource, needle) {
			t.Errorf("refresh.js still contains %q (%s)", needle, why)
		}
	}

	// Preact owns the last transient editor, so refresh is no longer coupled to
	// draft/UI state. Request generations remain the only stale-publish guard.
	mustNotRefresh("refreshSuspended", "UI drafts must not pause polling")
	mustNotRefresh("renameEditing", "toolbar picker state belongs to its Preact island")
	if got := strings.Count(dashboardAssets, "dashboardState.discardRequest(requestId, { responded });"); got < 2 {
		t.Errorf("post-request discard count = %d, want at least 2", got)
	}
	must(`id="dashboard-default-profile-control"`, "profile picker has a stable Preact host")
	must(`id="dashboard-default-sandbox-profile-control"`, "sandbox picker has a stable Preact host")
	must("mountToolbarProfilePickerFeature", "profile picker is mounted before row routing")
	must("createToolbarProfilePickerState", "profile picker draft has a state owner")
	for _, retired := range []string{
		"ignoreModals",
		"document.querySelector('.modal-overlay.show')",
		"document.querySelector('.action-menu.open')",
		`.slop-machine[data-status="pull-spinning"]`,
		"refresh({ force: true })",
	} {
		mustNotRefresh(retired, "Preact-owned interactions must not widen refresh suspension")
	}
	mustNot("refresh({ force: true })", "all callers use the single ordinary refresh path")

	// Keyed Preact rows survive publishes, so drag routing state must not leak
	// into the poll or recreate the old wedge class.
	mustNotRefresh("if (dndDragActive) return true;", "a Preact-owned drag must not suspend auto-refresh")
	mustNotRefresh("import { dndDragActive } from './dnd.js';", "refresh.js must not import drag routing state")

	// modalEditing was that shared, leak-prone boolean. It must stay
	// gone — reintroducing it brings the wedge class of bug back.
	mustNot("modalEditing", "the leak-prone shared modal/drag boolean must stay deleted")

	// dragend covers drop, Escape-cancel, and release-over-nothing; the
	// disposable binder repeats the same reset if its island is unmounted.
	must("listen(document, 'dragend'", "document dragend is registered through the disposable listener scope")
	must("dndDragActive = false;", "dragend and unmount cleanup reset member-drag routing state")
	must("for (const remove of removers.splice(0).reverse()) remove();", "unmount removes document drag listeners")
}

// The templates / summoning-circles list lives inside a Preact-owned management
// overlay. Its mutation handlers use the same ordinary refresh() path whether a
// child editor remains open or not; modal visibility no longer changes polling.
//
// This has no server path of its own, so — like the sibling guards in this
// package — it pins the wiring by string-searching the embedded modules. The
func TestDashboardHTML_RefreshOnCircleMutations(t *testing.T) {
	readMod := func(name string) string {
		t.Helper()
		b, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("embedded %s missing: %v", name, err)
		}
		return string(b)
	}

	// refresh() no longer has a force mode: every mutation uses the same path.
	refreshJS := readMod("js/refresh.js")
	if !strings.Contains(refreshJS, "export async function refresh()") {
		t.Error("js/refresh.js must expose the option-free refresh() path")
	}
	for _, retired := range []string{"ignoreModals", "opts.force", "refresh({ force: true })"} {
		if strings.Contains(refreshJS, retired) {
			t.Errorf("js/refresh.js still contains retired force plumbing %q", retired)
		}
	}

	// funcBody returns the source of the named function up to the next
	// top-level function, so an assertion is scoped to that handler and can't
	// be satisfied by a force call in a neighbour.
	tmplJS := readMod("js/management-actions.js")
	funcBody := func(sig string) string {
		t.Helper()
		start := strings.Index(tmplJS, sig)
		if start < 0 {
			t.Fatalf("js/management-actions.js: function %q not found", sig)
		}
		rest := tmplJS[start+len(sig):]
		end := len(rest)
		for _, next := range []string{"\nasync function ", "\nfunction "} {
			if i := strings.Index(rest, next); i >= 0 && i < end {
				end = i
			}
		}
		return rest[:end]
	}

	// Every circle-list mutation refreshes, including the paths that keep or
	// reopen a child editor after the mutation.
	for _, h := range []struct{ sig, why string }{
		{"async function installTemplateStarter(", "copying a starter must repaint the circle list while the picker stays open"},
		{"async function snapshotTemplateFromGroup(", "snapshot-a-group reopens the editor, so its refresh runs with a modal open"},
		{"async function saveTemplate(", "saving a circle must repaint the list at once"},
		{"async function removeTemplate(", "deleting a circle must drop the card at once"},
		{"async function importTemplate(", "importing a circle must show it in the list at once"},
		{"async function duplicateTemplate(", "duplicating a circle must show the copy in the list at once"},
	} {
		body := funcBody(h.sig)
		if !strings.Contains(body, "refresh()") {
			t.Errorf("%s must call refresh() — %s", h.sig, h.why)
		}
		if strings.Contains(body, "force: true") {
			t.Errorf("%s still uses the retired force-refresh option", h.sig)
		}
	}
}
