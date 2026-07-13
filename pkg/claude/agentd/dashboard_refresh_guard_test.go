package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's auto-refresh used to wedge permanently after a
// drag-and-drop retire because legacy rendering could detach the dragged row
// before dragend reset its global suspension flag. Preact now owns keyed group
// rows, so refresh stays live throughout a gesture and the flag is routing
// state only.
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

	// A single predicate is the source of truth for refresh suspension.
	must("function refreshSuspended(", "the single refresh-suspension predicate")

	// refresh() must consult it once up front and after each fetch/parse
	// phase — otherwise a refresh that began before a drag opened resumes
	// mid-gesture and re-renders underneath it. Every check threads the force
	// flag (see the force-refresh guard below). Post-request exits also settle
	// the shared store token instead of leaving its poll state pending.
	if got := strings.Count(dashboardAssets, "if (refreshSuspended({ ignoreModals: force })) {"); got != 3 {
		t.Errorf("refresh() suspension guard count = %d, want 3", got)
	}
	if got := strings.Count(dashboardAssets, "dashboardState.discardRequest(requestId, { responded });"); got < 2 {
		t.Errorf("post-request discard count = %d, want at least 2", got)
	}

	// Modal suspension is derived from the DOM, not a hand-maintained
	// boolean: a flag has to be reset on every modal close path or it
	// leaks and wedges auto-refresh. The DOM cannot leak. A force-refresh
	// opts out of ONLY this modal check (ignoreModals) — the rename/menu guards
	// still hold — so a mutation fired from inside a modal can
	// repaint the list behind it without reopening the wedge class of bug.
	must("if (!ignoreModals && document.querySelector('.modal-overlay.show')) return true;", "modal suspension is DOM-derived and force-skippable, not a leakable flag")

	// Keyed Preact rows survive publishes, so drag routing state must not leak
	// into refreshSuspended or recreate the old wedge class.
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

// The templates / summoning-circles list lives inside a .manage-overlay that is
// deliberately NOT refresh-suspended, but its mutating actions fire from CHILD
// .modal-overlay dialogs (the starters picker, the editor) that ARE — so a plain
// refresh() from those handlers is dropped by the modal-suspend guard and the
// circle list behind the dialog stays stale until the human closes and reopens
// the whole view (the reported bug). The fix threads a `force` flag through
// refresh() → refreshSuspended({ignoreModals}) that skips ONLY the modal check,
// and every circle-list mutation calls refresh({ force: true }).
//
// This has no server path of its own, so — like the sibling guards in this
// package — it pins the wiring by string-searching the embedded modules. The
// ignoreModals mechanic itself is guarded by TestDashboardHTML_RefreshGuardCannotWedge.
func TestDashboardHTML_ForceRefreshOnCircleMutations(t *testing.T) {
	readMod := func(name string) string {
		t.Helper()
		b, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("embedded %s missing: %v", name, err)
		}
		return string(b)
	}

	// refresh() takes the options object and derives force from it. The
	// `opts.force` read (rather than a positional boolean) is what makes it
	// safe when refresh is used bare as an event callback — an Event has no
	// .force, so it degrades to a normal refresh.
	refreshJS := readMod("js/refresh.js")
	for _, needle := range []string{
		"export async function refresh(opts = {})",
		"const force = !!(opts && opts.force);",
	} {
		if !strings.Contains(refreshJS, needle) {
			t.Errorf("js/refresh.js missing %q — force-refresh plumbing regressed", needle)
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

	// Every circle-list mutation force-refreshes. installStarter (copy a bundled
	// starter) and submitFromGroup (snapshot a group, then reopen the editor)
	// are the genuinely-broken cases — a modal stays open across the refresh;
	// the editor/delete/import handlers force too so the behaviour is uniform.
	for _, h := range []struct{ sig, why string }{
		{"async function installTemplateStarter(", "copying a starter must repaint the circle list while the picker stays open"},
		{"async function snapshotTemplateFromGroup(", "snapshot-a-group reopens the editor, so its refresh runs with a modal open"},
		{"async function saveTemplate(", "saving a circle must repaint the list at once"},
		{"async function removeTemplate(", "deleting a circle must drop the card at once"},
		{"async function importTemplate(", "importing a circle must show it in the list at once"},
		{"async function duplicateTemplate(", "duplicating a circle must show the copy in the list at once"},
	} {
		if !strings.Contains(funcBody(h.sig), "refresh({ force: true })") {
			t.Errorf("%s must call refresh({ force: true }) — %s", h.sig, h.why)
		}
	}
}
