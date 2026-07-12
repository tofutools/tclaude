package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's 5s auto-refresh used to wedge permanently after a
// drag-and-drop retire: an in-flight refresh that resumed mid-drag
// re-rendered the DOM, detached the dragged row, and the dragend event
// dispatched on that detached node never bubbled to the document-level
// handler — so the suspension flag was never reset and every later
// refresh() short-circuited forever.
//
// The fix lives entirely in dashboard.html's embedded JS, so there is
// no server code path a flow test can exercise. This guards the shape
// of that fix against a future refactor of the file silently undoing
// it. See the PR for the manual browser repro.
func TestDashboardHTML_RefreshGuardCannotWedge(t *testing.T) {
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
	// opts out of ONLY this modal check (ignoreModals) — the drag/rename/
	// menu guards still hold — so a mutation fired from inside a modal can
	// repaint the list behind it without reopening the wedge class of bug.
	must("if (!ignoreModals && document.querySelector('.modal-overlay.show')) return true;", "modal suspension is DOM-derived and force-skippable, not a leakable flag")

	// The drag has its own dedicated flag and does NOT share the modal
	// suspension — a single shared boolean let a drag and a modal
	// clobber each other's reset.
	must("if (dndDragActive) return true;", "a drag in flight suspends auto-refresh on its own flag")

	// modalEditing was that shared, leak-prone boolean. It must stay
	// gone — reintroducing it brings the wedge class of bug back.
	mustNot("modalEditing", "the leak-prone shared modal/drag boolean must stay deleted")

	// dragend is the one guaranteed reset for every drag-end outcome
	// (drop, Escape-cancel, release-over-nothing). It must clear the
	// drag flag, and must do so before any DOM cleanup that could throw.
	dragend := dashboardAssets[strings.Index(dashboardAssets, "addEventListener('dragend'"):]
	dragend = dragend[:strings.Index(dragend, "});")]
	if !strings.Contains(dragend, "dndDragActive = false;") {
		t.Errorf("dragend handler must reset dndDragActive = false")
	}
	if strings.Index(dragend, "dndDragActive = false;") > strings.Index(dragend, "classList.remove") {
		t.Errorf("dragend must clear dndDragActive before any DOM cleanup that could throw")
	}
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
	tmplJS := readMod("js/modal-templates.js")
	funcBody := func(sig string) string {
		t.Helper()
		start := strings.Index(tmplJS, sig)
		if start < 0 {
			t.Fatalf("js/modal-templates.js: function %q not found", sig)
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
		{"async function installStarter(", "copying a starter must repaint the circle list while the picker stays open"},
		{"async function submitFromGroup(", "snapshot-a-group reopens the editor, so its refresh runs with a modal open"},
		{"async function submitTemplateEditor(", "saving a circle must repaint the list at once"},
		{"async function deleteTemplate(", "deleting a circle must drop the card at once"},
		{"async function submitTemplateImport(", "importing a circle must show it in the list at once"},
		{"async function submitDuplicate(", "duplicating a circle must show the copy in the list at once"},
	} {
		if !strings.Contains(funcBody(h.sig), "refresh({ force: true })") {
			t.Errorf("%s must call refresh({ force: true }) — %s", h.sig, h.why)
		}
	}
}
