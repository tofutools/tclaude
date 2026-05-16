package agentd

import (
	"strings"
	"testing"
)

// Every drag-and-drop agent move on the dashboard must pop a
// confirmation modal first, so an accidental drag cannot mutate state.
// The gates live entirely in dashboard.html's embedded JS — each
// runDnd* function awaits a modal as its first step — so there is no
// server code path a flow test can exercise, and the repo has no JS
// test runner. Following the established pattern of the other
// dashboard_*_test.go structural guards (refresh-guard, context-meter,
// sort, wtsync), this pins the shape of the confirmation gates so a
// future refactor of the file cannot silently drop one. The seven
// operations were also exercised by hand in a browser (confirm AND
// cancel) — see the PR.
//
// dndFuncBody returns the source span of a single runDnd* function:
// from its `async function` keyword up to the next runDnd* definition
// (or bindTabs() for the last one). The trailing slice may include the
// next function's doc comment, which is harmless — none of the needles
// below appear in those comments.
func dndFuncBody(t *testing.T, name string) string {
	t.Helper()
	// Source order in dashboard.html; the marker after each function
	// is the start of the next span.
	order := []string{
		"runDndClone", "runDndMove", "runDndAddToGroup",
		"runDndRemoveFromGroup", "runDndRetire", "runDndPromoteToUngrouped",
		"runDndReinstate",
	}
	start := strings.Index(dashboardHTML, "async function "+name+"(")
	if start < 0 {
		t.Fatalf("dashboard.html: %s not found", name)
	}
	end := strings.Index(dashboardHTML, "bindTabs();")
	for i, n := range order {
		if n == name && i+1 < len(order) {
			next := strings.Index(dashboardHTML, "async function "+order[i+1]+"(")
			if next > start {
				end = next
			}
			break
		}
	}
	if end <= start {
		t.Fatalf("dashboard.html: could not bound %s", name)
	}
	return dashboardHTML[start:end]
}

// TestDashboardHTML_DndOperationsConfirm asserts each of the six
// non-retire drag operations is gated behind confirmModal(), and that
// the gate precedes any daemon call or optimistic snapshot mutation.
func TestDashboardHTML_DndOperationsConfirm(t *testing.T) {
	// The six runDnd* functions that gate on the shared confirmModal.
	// runDndRetire is deliberately excluded — it predates this change
	// and uses the richer retireConfirm modal (asserted separately
	// below).
	for _, name := range []string{
		"runDndClone", "runDndMove", "runDndAddToGroup",
		"runDndRemoveFromGroup", "runDndPromoteToUngrouped", "runDndReinstate",
	} {
		body := dndFuncBody(t, name)
		confirm := strings.Index(body, "await confirmModal({")
		if confirm < 0 {
			t.Errorf("%s: missing `await confirmModal({` — operation is not gated", name)
			continue
		}
		// The cancel path must bail without proceeding.
		if !strings.Contains(body, "if (!confirmed) { await refresh(); return; }") {
			t.Errorf("%s: cancel path must `await refresh(); return;` without running the op", name)
		}
		// The confirm must come before the first daemon round-trip.
		if fetch := strings.Index(body, "await fetch("); fetch >= 0 && confirm > fetch {
			t.Errorf("%s: confirmModal must precede the first fetch() — found fetch at %d, confirm at %d", name, fetch, confirm)
		}
	}

	// runDndMove is the only optimistic operation: it splices
	// lastSnapshot before the round-trip. The confirm must precede that
	// splice too, or a cancelled move still flickers the UI.
	move := dndFuncBody(t, "runDndMove")
	confirm := strings.Index(move, "await confirmModal({")
	splice := strings.Index(move, ".splice(")
	if confirm < 0 || splice < 0 {
		t.Fatalf("runDndMove: expected both a confirmModal and a .splice( — confirm=%d splice=%d", confirm, splice)
	}
	if confirm > splice {
		t.Errorf("runDndMove: optimistic .splice() (at %d) must not run before the confirm (at %d)", splice, confirm)
	}

	// runDndRetire keeps its richer retireConfirm modal (shutdown
	// checkbox and all) so a retire-by-drag asks the identical question
	// as the per-row retire button. It must stay gated.
	retire := dndFuncBody(t, "runDndRetire")
	if !strings.Contains(retire, "await retireConfirm({") {
		t.Error("runDndRetire: must stay gated behind retireConfirm({")
	}
	if rc, fetch := strings.Index(retire, "await retireConfirm({"), strings.Index(retire, "await fetch("); rc >= 0 && fetch >= 0 && rc > fetch {
		t.Error("runDndRetire: retireConfirm must precede the retire fetch()")
	}
}
