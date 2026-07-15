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
// The wiring spans the native MemberMenu/MemberActions components and the
// dispatcher case that routes it through the
// retireConfirm modal (row-actions.js). The repo has no JS test runner,
// so — following the established dashboard_*_test.go structural guards
// (dnd-confirm, agents-tab, slop-machine) — this pins the shape of that
// wiring so a refactor can't silently drop the button or unhook it from
// the confirmation modal. The behaviour itself was exercised by hand in
// a browser; the backend retire endpoint has its own flow tests
// (retire_shutdown / retire_worktree / groups_retire).

// helpersFuncBody returns the source span of a column-0 function in the
// embedded dashboard JS — from its `function <name>(` keyword to its own
// closing brace. Dashboard modules are native ES modules, so functions sit
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
	// 1. The native menu dispatches the retire-agent path with the conv-keyed
	// selector + label the dispatcher reads in both grouped and ungrouped arms.
	tmpl := helpersFuncBody(t, "MemberMenu")
	for _, needle := range []string{
		`selector="conv" act="retire-agent"`,
		`className="warn" regular="retire" wizard="banish"`,
		`group=${group} act="remove-member"`,
		`act="delete-agent" className="danger"`,
	} {
		if !strings.Contains(tmpl, needle) {
			t.Errorf("MemberMenu: missing %q", needle)
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

	// 4. The shared retireAgentInteractive gates the retire on retireConfirm
	//    — the SAME modal the drag-onto-Retired gesture uses — by handing it
	//    a `perform` callback that runs the /retire POST. retireConfirm only
	//    invokes perform after the human confirms (and keeps the modal open
	//    with a spinner while it runs, see TestDashboardHTML_SingleRetireSpinner),
	//    so neither an accidental click nor a stray palette pick can retire
	//    without confirmation.
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
	if !strings.Contains(fnBody, "perform:") {
		t.Error("retireAgentInteractive: must delegate the retire work to a `perform:` callback so retireConfirm runs it only after confirmation")
	}
	fetch := strings.Index(fnBody, "/retire")
	if fetch < 0 {
		t.Fatal("retireAgentInteractive: missing the /retire POST")
	}
	// The POST must live inside the retireConfirm({…}) call (the perform
	// callback), never before it — i.e. retireConfirm precedes the fetch.
	if confirm > fetch {
		t.Errorf("retireAgentInteractive: retireConfirm (at %d) must precede the /retire POST (at %d)", confirm, fetch)
	}
}

// TestDashboardHTML_RetireIconButtonWired pins the top-level 🗑 trash icon —
// the quick-control twin of the ⚙-menu retire item. It rides beside the cog
// in the row-action cluster and dispatches the SAME retire-agent path
// (conv-keyed, no data-agent) so the delegated handler routes it through the
// identical retireAgentInteractive → retireConfirm flow guarded above. This
// pins the icon's shape + its mount in both active-agent row renderers so a
// refactor can't silently drop it or key it wrong.
func TestDashboardHTML_RetireIconButtonWired(t *testing.T) {
	tmpl := helpersFuncBody(t, "MemberActions")
	for _, needle := range []string{
		`data-act="retire-agent"`,
		`data-conv=${member.conv_id}`,
		`data-label=${member.title || member.conv_id}`,
		`class="icon-btn warn"`, // reversible-demotion semantics (warn), not danger
		`<${TrashIcon} />`,      // renders the trash glyph, not a text label
		`aria-label="Retire agent"`,
	} {
		if !strings.Contains(tmpl, needle) {
			t.Errorf("retireIconButton: missing %q", needle)
		}
	}
	// The glyph itself is a module-level inline SVG (like the eye pair),
	// referenced above via ${TRASH_SVG}. Pin its definition + the CSS hook
	// so the icon can't silently become an empty span.
	for _, needle := range []string{
		`function TrashIcon()`,
		`class="trash-ico"`,
		`<svg`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets: missing %q (the TRASH_SVG glyph)", needle)
		}
	}
	// It must stay conv-keyed (no data-agent): a stable agent_id would
	// resolve even for a dangling agent and silently demote the orphan
	// instead of offering to remove it — the same reason the menu button is
	// conv-keyed (retireMemberButton / JOH-322).
	if strings.Contains(tmpl, "data-agent=") {
		t.Error("retireIconButton: must stay conv-keyed (no data-agent) — see retireMemberButton / JOH-322")
	}
	// The same native row action component serves grouped and ungrouped rows,
	// and its ActionMenu retains the menu twin.
	if !strings.Contains(tmpl, "<${ActionMenu}") || !strings.Contains(tmpl, "<${MemberMenu}") {
		t.Error("MemberActions: the menu retire twin must stay beside the top-level shortcut")
	}
}

// TestDashboardHTML_SingleRetireSpinner pins the in-flight feedback for the
// SINGLE-agent retire (the bulk-retire preview's spinner is guarded
// separately in dashboard_retire_preview_test.go). retireConfirm runs its
// caller's `perform` with the modal still open and the OK button swapped to
// a .btn-spinner ("Retiring…"), so a retire that takes a beat doesn't look
// ignored. This guards that wiring so a refactor can't silently drop it.
func TestDashboardHTML_SingleRetireSpinner(t *testing.T) {
	start := strings.Index(dashboardAssets, "function retireConfirm(")
	if start < 0 {
		t.Fatal("refresh.js: `function retireConfirm(` not found")
	}
	fnBody, _, found := strings.Cut(dashboardAssets[start:], "\n}\n")
	if !found {
		t.Fatal("refresh.js: could not bound retireConfirm")
	}
	// The function must take the perform callback the callers feed it…
	if !strings.Contains(fnBody, "perform}") {
		t.Error("retireConfirm: must accept a `perform` callback (`{label, conv, perform}`)")
	}
	// …and on confirm show the in-button spinner + aria-busy while it runs,
	// then clear the busy state so the reusable modal opens clean next time.
	for _, needle := range []string{
		`okBtn.setAttribute('aria-busy', 'true')`, // busy flag on confirm
		`class="btn-spinner"`,                     // in-button spinner
		`Retiring…`,                               // busy label
		`okBtn.removeAttribute('aria-busy')`,      // cleared when idle again
	} {
		if !strings.Contains(fnBody, needle) {
			t.Errorf("retireConfirm: missing %q — confirm must give in-flight feedback", needle)
		}
	}
	// The spinner needs its CSS rule (shared with the bulk-retire button).
	if !strings.Contains(dashboardAssets, ".btn-spinner {") {
		t.Error("dashboard assets: missing the .btn-spinner CSS rule")
	}
}
