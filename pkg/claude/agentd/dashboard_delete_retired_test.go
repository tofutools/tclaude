package agentd

import (
	"strings"
	"testing"
)

// JOH-31: the dashboard's "Delete retired agents…" tool — reachable from
// the command palette and the Groups ⚙ menu — opens a PREVIEW modal
// (openDeleteRetiredPreview, refresh.js) that lists every retired agent
// (all ticked), offers live title/age filters and a per-row opt-out, and
// on submit POSTs the EXPLICIT list of conv-ids that are BOTH ticked AND
// visible to /api/cleanup/agents {mode:"delete"}.
//
// The repo has no JS test runner, so — following the established
// dashboard_*_test.go structural guards — this pins the wiring across the
// embedded HTML + JS so a refactor can't silently drop the preview, the
// opt-out, or (crucially) the ticked-AND-visible explicit-list POST that
// makes "what was previewed" == "what is deleted" (the operator's
// load-bearing invariant). The delete backend path has its own flow tests
// (cleanup_flow_test.go: TestCleanup_Agents_DeleteRetired_ExplicitSubsetOnly /
// TestCleanup_Agents_DeleteRetiredAgent).
func TestDashboardHTML_DeleteRetiredWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// 1. Markup: the overlay rides on .modal-overlay (shared backdrop), with the
	//    candidate list, the per-row opt-out
	//    toolbar (select-all/none + the title filter + the age floor), the
	//    delete-worktrees toggle, and the submit button the JS drives.
	must(`id="delete-retired-modal"`, "the delete-retired overlay exists")
	must(`class="modal-overlay" id="delete-retired-modal"`,
		"the preview uses the shared modal backdrop")
	must(`id="delete-retired-list"`, "the preview has a candidate list")
	must(`id="delete-retired-search"`, "the preview has a title/id filter")
	must(`id="delete-retired-age"`, "the preview has an age-floor filter")
	must(`id="delete-retired-select-all"`, "the preview can select all candidates")
	must(`id="delete-retired-select-none"`, "the preview can clear the selection")
	must(`id="delete-retired-wt"`, "the preview has a delete-worktrees toggle")
	must(`id="delete-retired-submit"`, "the preview has a submit button")
	must(`id="delete-retired-cancel"`, "the preview has a cancel button")

	// 2. The driver is defined and exported, and BOTH affordances reach it:
	//    the command palette and the Groups ⚙ menu button.
	must("function openDeleteRetiredPreview(", "refresh.js defines the preview driver")
	must("openDeleteRetiredPreview,", "refresh.js exports the preview driver")
	must("run: () => openDeleteRetiredPreview()", "the palette command opens the preview")
	must(`id="delete-retired-open"`, "a Groups-menu button opens the preview for discoverability")
	must("$('#delete-retired-open').addEventListener('click', () => openDeleteRetiredPreview())",
		"the Groups-menu button is wired to the driver")

	// 3. The candidate list is seeded from the FULL retired endpoint
	//    (/api/retired with no paging params), all ticked by default — so the
	//    headline action targets the whole retired population and the human
	//    opts rows OUT. The snapshot now only ships ONE PAGE of retired[], so
	//    reading it would silently scope the bulk delete to the visible window;
	//    the modal must fetch the complete list.
	must("retired = await fetchListFull('retired')",
		"the preview seeds its candidates from the full retired endpoint, not the windowed snapshot")
	must("checked: true,", "retired candidates are ticked by default")

	// Bound the rest of the assertions to the driver's own body so a needle
	// can't be satisfied by an unrelated modal elsewhere in refresh.js.
	disp := dashboardAssets
	start := strings.Index(disp, "function openDeleteRetiredPreview(")
	if start < 0 {
		t.Fatal("refresh.js: function openDeleteRetiredPreview( not found")
	}
	fnBody, _, found := strings.Cut(disp[start:], "\n}\n")
	if !found {
		t.Fatal("refresh.js: could not bound openDeleteRetiredPreview")
	}

	// 4. THE load-bearing property (JOH-31): submit posts the conv-ids that
	//    are BOTH ticked AND visible (pass matchesFilter) — not merely
	//    c.checked. A row hidden by a filter is never deleted even if it was
	//    ticked before the filter narrowed. The list is sent verbatim to the
	//    delete tier; it is NOT a status filter the server re-resolves.
	for _, needle := range []string{
		"candidates.filter(c => c.checked && matchesFilter(c))", // ticked AND visible — the whole point
		"visibleChecked().map(c => c.agent_id || c.conv_id)",    // posted set (agent_id, conv_id fallback)
		"mode: 'delete'",        // the existing delete tier
		"'/api/cleanup/agents'", // to the cleanup endpoint, not a new one
	} {
		if !strings.Contains(fnBody, needle) {
			t.Errorf("openDeleteRetiredPreview: missing %q — the ticked-AND-visible explicit-list POST is the whole point", needle)
		}
	}
	// It must NOT post the bare c.checked set (the retire-preview behavior) —
	// that would delete rows the human filtered out of view.
	if strings.Contains(fnBody, "candidates.filter(c => c.checked).map") {
		t.Error("openDeleteRetiredPreview: must post ticked-AND-visible, not bare c.checked (a filtered-out row must not be deleted)")
	}
	// And it must not smuggle in a server-re-resolved status filter.
	if strings.Contains(fnBody, "?status=") {
		t.Error("openDeleteRetiredPreview: must POST the explicit conv list, not a ?status= filter the server re-resolves")
	}

	// 5. Both live filters re-render the list, and select-all/none act on the
	//    currently-FILTERED rows only (never ticking/unticking hidden rows).
	for _, needle := range []string{
		"searchEl.addEventListener('input', onSearch)",                        // title/id filter is live
		"ageEl.addEventListener('input', onAge)",                              // age floor is live
		"for (const c of candidates.filter(matchesFilter)) c.checked = true",  // select-all on filtered only
		"for (const c of candidates.filter(matchesFilter)) c.checked = false", // select-none on filtered only
	} {
		if !strings.Contains(fnBody, needle) {
			t.Errorf("openDeleteRetiredPreview: missing %q — filters/selection wiring broken", needle)
		}
	}

	// 6. Busy feedback: clicking submit shows an in-flight state (spinner +
	//    aria-busy) and renderFooter tears it back down on the error paths.
	for _, needle := range []string{
		`submitBtn.setAttribute('aria-busy', 'true')`,
		`class="btn-spinner"`,
		`submitBtn.removeAttribute('aria-busy')`,
	} {
		if !strings.Contains(fnBody, needle) {
			t.Errorf("openDeleteRetiredPreview: missing %q — submit must give in-flight feedback", needle)
		}
	}

	// 7. The submit button reads as DESTRUCTIVE — the cleanup-modal red
	//    `primary danger` variant — so a batch DELETE never looks benign.
	must(`id="delete-retired-submit" class="primary danger"`,
		"the batch-delete submit button carries the cleanup-modal danger (red) styling")
	must(".cleanup-modal .modal-buttons button.primary.danger {",
		"the cleanup-modal danger (red) button rule exists for the submit to bind to")

	// 8. The palette command is gated on ≥1 retired agent so it never offers
	//    a no-op, and carries the 🗑 icon (distinct from the ♻ retire ones).
	must("const retiredCount = snap.retired_total || 0",
		"the palette command gates on the snapshot's cheap retired total even when the full list is not fetched")
	must("if (retiredCount) {", "the command is only listed when there is at least one retired agent")
	must("icon: wiz('🗑', '🔥'), label: wiz('Delete retired agents…', 'Dispel banished familiars…')",
		"the palette command carries the distinct 🗑 delete label (arcane 🔥 Dispel in wizard mode)")
	must("keywords: 'delete purge retired cleanup remove wipe agents'", "the command's search keywords")
}
