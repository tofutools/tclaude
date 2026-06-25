package agentd

import (
	"strings"
	"testing"
)

// The command palette's per-group "Retire idle/offline agents in
// <group>" command opens a PREVIEW modal (openRetirePreview, refresh.js)
// rather than firing a status-filtered bulk retire the server re-resolves
// from live state. The modal lists precisely the matching members, lets
// the human opt agents out, and POSTs the EXPLICIT ticked conv-id list to
// /api/groups/{name}/retire {convs:[…]} — so the BE retires exactly what
// the human previewed.
//
// The repo has no JS test runner, so — following the established
// dashboard_*_test.go structural guards — this pins the shape of that
// wiring across the embedded HTML + JS so a refactor can't silently drop
// the preview, the opt-out, or (crucially) the explicit-list POST that
// makes "what was previewed" == "what is retired". The explicit-convs
// backend path has its own flow tests (groups_retire_flow_test.go:
// TestDashboardGroupRetire_ExplicitConvsSelection / *OverrideStatusQuery).
func TestDashboardHTML_RetirePreviewWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// 1. Markup: the overlay rides on .modal-overlay (shared backdrop +
	//    auto-refresh suspend), with the list, the per-row opt-out toolbar
	//    (select-all/none + filter), the shutdown toggle, and the submit
	//    button the JS drives.
	must(`id="retire-preview-modal"`, "the retire-preview overlay exists")
	must(`class="modal-overlay" id="retire-preview-modal"`,
		"the preview is a .modal-overlay so it suspends the 2s refresh while open (cohort can't shift)")
	must(`id="retire-preview-list"`, "the preview has a candidate list")
	must(`id="retire-preview-search"`, "the preview has a title/id filter")
	must(`id="retire-preview-select-all"`, "the preview can select all candidates")
	must(`id="retire-preview-select-none"`, "the preview can clear the selection")
	must(`id="retire-preview-shutdown"`, "the preview has a shut-down-sessions toggle")
	must(`id="retire-preview-submit"`, "the preview has a submit button")
	must(`id="retire-preview-cancel"`, "the preview has a cancel button")

	// 2. The driver is defined and exported, and the palette reaches it.
	must("function openRetirePreview(", "refresh.js defines the preview driver")
	must("openRetirePreview,", "refresh.js exports the preview driver")
	must("openRetirePreview(g.name, status)", "the palette opens the preview for the chosen cohort")

	// 3. The candidate list is built from the SAME snapshot-derived cohort
	//    the palette count uses (groupMembersByStatus), so the preview lists
	//    exactly the rows the human sees, all ticked by default.
	must("groupMembersByStatus(group, status).map(m => ({ ...m, checked: true }))",
		"the preview seeds its candidates from the matching cohort, all ticked")
	must("function groupMembersByStatus(",
		"groupMembersByStatus is the shared cohort builder")

	// 4. THE load-bearing property: submit posts the EXPLICIT ticked
	//    conv-id list (not a ?status= filter the server re-resolves), so
	//    the BE retires exactly what the human reviewed.
	disp := dashboardAssets
	start := strings.Index(disp, "function openRetirePreview(")
	if start < 0 {
		t.Fatal("refresh.js: function openRetirePreview( not found")
	}
	// Bound at the function's own column-0 closing brace.
	fnBody, _, found := strings.Cut(disp[start:], "\n}\n")
	if !found {
		t.Fatal("refresh.js: could not bound openRetirePreview")
	}
	for _, needle := range []string{
		"candidates.filter(c => c.checked).map(c => c.conv_id)", // the ticked list
		"JSON.stringify({ convs, shutdown: shutdownCb.checked })", // posted verbatim in the body
		"/api/groups/${encodeURIComponent(group)}/retire",        // to the group retire route
	} {
		if !strings.Contains(fnBody, needle) {
			t.Errorf("openRetirePreview: missing %q — the explicit-list POST is the whole point", needle)
		}
	}
	// And it must NOT fall back to the server-re-resolved status filter on
	// this path (no ?status= in the submit URL).
	if strings.Contains(fnBody, "?status=") {
		t.Error("openRetirePreview: must POST the explicit conv list, not a ?status= filter the server re-resolves")
	}

	// 5. Busy feedback: clicking submit must show an in-flight state (the
	//    spinner + aria-busy) so a click that takes a beat isn't mistaken
	//    for a no-op, and renderFooter must tear that busy state back down
	//    on the error paths.
	for _, needle := range []string{
		`submitBtn.setAttribute('aria-busy', 'true')`, // busy flag on click
		`class="btn-spinner"`,                          // in-button spinner
		`submitBtn.removeAttribute('aria-busy')`,       // cleared when ready again
	} {
		if !strings.Contains(fnBody, needle) {
			t.Errorf("openRetirePreview: missing %q — submit must give in-flight feedback", needle)
		}
	}
	must(".btn-spinner {", "the in-button spinner has a CSS rule")

	// 6. The submit button must read as DESTRUCTIVE — red, like the single
	//    -agent retire/delete confirms — so a batch retire signals it is
	//    really shutting agents down rather than looking like a benign
	//    primary action. Inside .cleanup-modal that's the `primary danger`
	//    red variant (confirm-danger's neutral cleanup-modal base would lose
	//    to the generic button rule at equal specificity); the red rule must
	//    exist for it to bind to.
	must(`id="retire-preview-submit" class="primary danger"`,
		"the batch-retire submit button carries the cleanup-modal danger (red) styling")
	must(".cleanup-modal .modal-buttons button.primary.danger {",
		"the cleanup-modal danger (red) button rule exists for the submit to bind to")
}
