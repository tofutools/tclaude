package agentd

import (
	"strings"
	"testing"
)

// Every drag-and-drop agent move on the dashboard must pop a
// confirmation modal first, so an accidental drag cannot mutate state.
// The gates live entirely in the dashboard's embedded JS modules —
// each runDnd* function awaits a modal as its first step — so there is
// no server code path a flow test can exercise, and the repo has no JS
// test runner.
//
// Following the established pattern of the other dashboard_*_test.go
// structural guards (refresh-guard, context-meter, sort, wtsync), this
// is a STRUCTURAL guard: it pins the *shape* of the confirmation gates
// — gate present, gate before the first daemon call, optimistic
// mutation after the gate, and a finally-resync on every exit path —
// so a future refactor cannot silently drop one. It is not a
// behavioural test; the seven operations were exercised by hand in a
// browser (confirm AND cancel) — see the PR.

// dndFuncBody returns the source span of a single runDnd* function: the
// text from its `async function` keyword to its closing brace. The
// span is bounded by the next runDnd* definition (or dnd.js's export
// block for the last one) and then trimmed back to the function's own
// closing brace, so the next function's doc comment is excluded.
func dndFuncBody(t *testing.T, name string) string {
	t.Helper()
	// Source order in dnd.js; the definition after each function
	// bounds its span.
	order := []string{
		"runDndClone", "runDndMove", "runDndAddToGroup",
		"runDndRemoveFromGroup", "runDndRetire", "runDndPromoteToUngrouped",
		"runDndReinstate",
	}
	start := strings.Index(dashboardAssets, "async function "+name+"(")
	if start < 0 {
		t.Fatalf("dashboard assets: %s not found", name)
	}
	// Search the end marker strictly AFTER `start` so an identical
	// token earlier in the file can never truncate the slice. The last
	// runDnd* function is bounded by dnd.js's trailing export block.
	rest := dashboardAssets[start+1:]
	endRel := strings.Index(rest, "\nexport {")
	for i, n := range order {
		if n == name && i+1 < len(order) {
			if next := strings.Index(rest, "async function "+order[i+1]+"("); next >= 0 {
				endRel = next
			}
			break
		}
	}
	if endRel < 0 {
		t.Fatalf("dashboard assets: could not bound %s", name)
	}
	body := dashboardAssets[start : start+1+endRel]
	// Trim the trailing whitespace + next function's doc comment back
	// to this function's own closing brace ("\n}" — dnd.js is a native
	// ES module, so the runDnd* functions sit at column 0; every deeper
	// brace is indented further, doc comments never are).
	if i := strings.LastIndex(body, "\n}"); i >= 0 {
		body = body[:i+len("\n}")]
	}
	return body
}

// TestDashboardHTML_DndOperationsConfirm asserts each of the six
// non-retire drag operations is gated behind confirmModal(), that the
// gate (and its cancel guard) precede any daemon call or optimistic
// snapshot mutation, and that every exit path re-syncs the dashboard.
func TestDashboardHTML_DndOperationsConfirm(t *testing.T) {
	// The six runDnd* functions that gate on the shared confirmModal.
	// runDndRetire is deliberately excluded — it predates this change
	// and uses the richer retireConfirm modal (asserted separately
	// below).
	gated := []string{
		"runDndClone", "runDndMove", "runDndAddToGroup",
		"runDndRemoveFromGroup", "runDndPromoteToUngrouped", "runDndReinstate",
	}
	for _, name := range gated {
		body := dndFuncBody(t, name)

		confirm := strings.Index(body, "await confirmModal({")
		if confirm < 0 {
			t.Errorf("%s: missing `await confirmModal({` — operation is not gated", name)
			continue
		}
		// The cancel path must bail (refresh + return) without running
		// the op.
		cancel := strings.Index(body, "if (!confirmed) { await refresh(); return; }")
		if cancel < 0 {
			t.Errorf("%s: cancel path must `await refresh(); return;` without running the op", name)
			continue
		}
		// Both the confirm AND its cancel guard must precede the first
		// daemon round-trip — checking only confirmModal's position
		// would still pass a refactor that fetched, then checked
		// !confirmed.
		if fetch := strings.Index(body, "await fetch("); fetch >= 0 {
			if confirm > fetch {
				t.Errorf("%s: confirmModal (at %d) must precede the first fetch() (at %d)", name, confirm, fetch)
			}
			if cancel > fetch {
				t.Errorf("%s: cancel guard (at %d) must precede the first fetch() (at %d)", name, cancel, fetch)
			}
		}
		// Every gated function must re-sync on completion. The confirm
		// modal suspends auto-refresh while open, and the dragend-fired
		// refresh() bails for the same reason — so a `finally { await
		// refresh() }` is what guarantees the dashboard is not left
		// showing stale state on ANY exit path (including a confirmed-
		// then-aborted op that hits an early return).
		if !strings.Contains(body, "} finally {") {
			t.Errorf("%s: must re-sync via a `finally { await refresh() }` block on every exit path", name)
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
	// The cancel guard must precede the splice too — not just the
	// confirm. A refactor that moved `if (!confirmed)` to between the
	// splice and the first fetch would still pass the confirm-before-
	// splice check above (confirmModal stays first), yet a cancelled
	// move would run the optimistic splice and flicker the UI before
	// bailing.
	cancel := strings.Index(move, "if (!confirmed) { await refresh(); return; }")
	if cancel < 0 {
		t.Fatalf("runDndMove: cancel guard `if (!confirmed) { await refresh(); return; }` not found")
	}
	if cancel > splice {
		t.Errorf("runDndMove: cancel guard (at %d) must precede the optimistic .splice() (at %d) — a cancelled move must not run the splice", cancel, splice)
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
