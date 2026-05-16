package agentd

import (
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
		if !strings.Contains(dashboardHTML, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardHTML, needle) {
			t.Errorf("dashboard.html still contains %q (%s)", needle, why)
		}
	}

	// A single predicate is the source of truth for refresh suspension.
	must("function refreshSuspended()", "the single refresh-suspension predicate")

	// refresh() must consult it TWICE: once up front, and again AFTER
	// the /api/snapshot await — otherwise a refresh that began before a
	// drag opened resumes mid-gesture and re-renders underneath it.
	must("if (refreshSuspended()) {", "refresh() guards at the top")
	must("if (refreshSuspended()) return;", "refresh() re-checks after the snapshot fetch, before touching the DOM")

	// Modal suspension is derived from the DOM, not a hand-maintained
	// boolean: a flag has to be reset on every modal close path or it
	// leaks and wedges auto-refresh. The DOM cannot leak.
	must("document.querySelector('.modal-overlay.show')", "modal suspension is DOM-derived, not a leakable flag")

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
	dragend := dashboardHTML[strings.Index(dashboardHTML, "addEventListener('dragend'"):]
	dragend = dragend[:strings.Index(dragend, "});")]
	if !strings.Contains(dragend, "dndDragActive = false;") {
		t.Errorf("dragend handler must reset dndDragActive = false")
	}
	if strings.Index(dragend, "dndDragActive = false;") > strings.Index(dragend, "classList.remove") {
		t.Errorf("dragend must clear dndDragActive before any DOM cleanup that could throw")
	}
}
