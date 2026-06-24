package agentd

import (
	"strings"
	"testing"
)

// The per-agent ⚙ options menu carries a "retire" button — the
// menu-button twin of dragging the row onto the virtual Retired group.
// Retiring used to be reachable ONLY by that drag, which is a long haul
// once many groups and agents are on screen (and can need scrolling).
// The button dispatches the SAME retire-agent path and pops the SAME
// retireConfirm modal as the drag, so both ask the identical question.
//
// The wiring spans three embedded JS modules — the button template
// (helpers.js), its inclusion in both active-agent menu builders
// (helpers.js), and the dispatcher case that routes it through the
// retireConfirm modal (row-actions.js). The repo has no JS test runner,
// so — following the established dashboard_*_test.go structural guards
// (dnd-confirm, agents-tab, slop-machine) — this pins the shape of that
// wiring so a refactor can't silently drop the button or unhook it from
// the confirmation modal. The behaviour itself was exercised by hand in
// a browser; the backend retire endpoint has its own flow tests
// (retire_shutdown / retire_worktree / groups_retire).

// helpersFuncBody returns the source span of a column-0 function in the
// embedded dashboard JS — from its `function <name>(` keyword to its own
// closing brace. helpers.js is a native ES module, so its functions sit
// at column 0 and the next `\nfunction ` bounds the span; the trailing
// LastIndex("\n}") trims back to this function's own closing brace,
// excluding the next function's doc comment.
func helpersFuncBody(t *testing.T, name string) string {
	t.Helper()
	start := strings.Index(dashboardAssets, "\nfunction "+name+"(")
	if start < 0 {
		t.Fatalf("dashboard assets: function %s not found", name)
	}
	rest := dashboardAssets[start+1:]
	end := strings.Index(rest, "\nfunction ")
	if end < 0 {
		t.Fatalf("dashboard assets: could not bound function %s", name)
	}
	body := rest[:end]
	if i := strings.LastIndex(body, "\n}"); i >= 0 {
		body = body[:i+len("\n}")]
	}
	return body
}

func TestDashboardHTML_RetireButtonWired(t *testing.T) {
	// 1. The button template exists and dispatches the retire-agent path
	//    with the conv + label the dispatcher reads.
	tmpl := helpersFuncBody(t, "retireMemberButton")
	for _, needle := range []string{
		`data-act="retire-agent"`,
		`data-conv="`,
		`data-label="`,
	} {
		if !strings.Contains(tmpl, needle) {
			t.Errorf("retireMemberButton: missing %q", needle)
		}
	}

	// 2. Both active-agent menu builders include the retire button. These
	//    are the only two renderers for an agent that can be retired (a
	//    real-group member and a virtual-Ungrouped row); the retired and
	//    conversation renderers must NOT offer it.
	for _, fn := range []string{"memberActions", "ungroupedMemberActions"} {
		if !strings.Contains(helpersFuncBody(t, fn), "retireMemberButton(m)") {
			t.Errorf("%s: missing retireMemberButton(m) — the retire option is not in this menu", fn)
		}
	}

	// 3. The dispatcher's retire-agent case delegates to the shared
	//    retireAgentInteractive flow — the SAME flow the command palette's
	//    "Retire agent: <name>" runs — so both entry points ask the
	//    identical question.
	disp := dashboardAssets
	caseIdx := strings.Index(disp, "case 'retire-agent': {")
	if caseIdx < 0 {
		t.Fatal("row-actions.js: `case 'retire-agent': {` dispatcher case not found")
	}
	// Bound the case body at the next `case ` so a later string can't
	// satisfy the assertion for this one.
	caseBody := disp[caseIdx:]
	if next := strings.Index(caseBody[len("case 'retire-agent': {"):], "\n        case "); next >= 0 {
		caseBody = caseBody[:len("case 'retire-agent': {")+next]
	}
	if !strings.Contains(caseBody, "retireAgentInteractive(conv, label)") {
		t.Error("retire-agent case: must delegate to retireAgentInteractive(conv, label)")
	}

	// 4. The shared retireAgentInteractive gates on retireConfirm — the
	//    SAME modal the drag-onto-Retired gesture uses — and only POSTs
	//    after a non-null choice, so neither an accidental click nor a
	//    stray palette pick can retire without confirmation.
	start := strings.Index(disp, "async function retireAgentInteractive(")
	if start < 0 {
		t.Fatal("refresh.js: `async function retireAgentInteractive(` not found")
	}
	// Bound at the function's own column-0 closing brace — nested braces in
	// a native ES module are indented, so the first "\n}\n" ends it.
	fnBody, _, found := strings.Cut(disp[start:], "\n}\n")
	if !found {
		t.Fatal("refresh.js: could not bound retireAgentInteractive")
	}
	confirm := strings.Index(fnBody, "await retireConfirm({")
	if confirm < 0 {
		t.Fatal("retireAgentInteractive: must gate on `await retireConfirm({`")
	}
	if !strings.Contains(fnBody, "if (!choice) return;") {
		t.Error("retireAgentInteractive: must bail on a null choice (`if (!choice) return;`)")
	}
	fetch := strings.Index(fnBody, "/retire")
	if fetch >= 0 && confirm > fetch {
		t.Errorf("retireAgentInteractive: retireConfirm (at %d) must precede the /retire POST (at %d)", confirm, fetch)
	}
}
