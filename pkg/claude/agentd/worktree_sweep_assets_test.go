package agentd

import (
	"strings"
	"testing"
)

// The group cog's "🧹 cleanup worktrees…" command opens the repo-wide
// worktree janitor modal (openWorktreeCleanup, refresh.js): it loads the
// candidate set from GET /api/groups/{name}/worktrees, lets the human
// edit the selection (per-row, category mass-toggle chips, select-all/
// none, filter, live rescan), and POSTs the EXPLICIT ticked path list to
// /api/worktrees/cleanup.
//
// The repo has no JS test runner, so — following the established
// dashboard_*_test.go structural guards (TestDashboardHTML_RetirePreviewWired)
// — this pins the shape of that wiring across the embedded HTML + JS so a
// refactor can't silently drop the modal, the mass-toggle/rescan
// controls, or the explicit-list POST. The backend has its own flow tests
// (worktree_sweep_flow_test.go).
func TestDashboardHTML_WorktreeCleanupWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// 1. Markup: the overlay rides on .modal-overlay (shared backdrop +
	//    auto-refresh suspend) with the list, the opt-out toolbar
	//    (select-all/none + filter), the live rescan button, the category
	//    mass-toggle row, the delete-branches toggle and the submit button.
	must(`class="modal-overlay" id="worktree-cleanup-modal"`,
		"the modal is a .modal-overlay so it suspends the 2s refresh while open")
	must(`id="worktree-cleanup-list"`, "the modal has a candidate list")
	must(`id="worktree-cleanup-search"`, "the modal has a path/branch filter")
	must(`id="worktree-cleanup-select-all"`, "the modal can select all candidates")
	must(`id="worktree-cleanup-select-none"`, "the modal can clear the selection")
	must(`id="worktree-cleanup-rescan"`, "the modal has a live rescan button")
	must(`id="worktree-cleanup-categories"`, "the modal has the category mass-toggle row")
	must(`id="worktree-cleanup-branches"`, "the modal has the delete-branches toggle")
	must(`id="worktree-cleanup-submit"`, "the modal has a submit button")
	must(`id="worktree-cleanup-cancel"`, "the modal has a cancel button")

	// 2. The driver is defined and exported, and both entry points reach it.
	must("async function openWorktreeCleanup(", "refresh.js defines the driver")
	must("openWorktreeCleanup,", "refresh.js exports the driver")
	must("openWorktreeCleanup(group)", "row-actions / palette open the modal for the group")
	must(`data-act="cleanup-worktrees-group"`, "the group cog has the cleanup-worktrees button")
	must("case 'cleanup-worktrees-group'", "row-actions dispatches the cog button")

	// 3. The load-bearing properties: discovery is a per-group GET and the
	//    submit POSTs the EXPLICIT ticked path list (not a filter the server
	//    re-resolves), so what the human previewed is exactly what is removed.
	must("/api/groups/${encodeURIComponent(group)}/worktrees",
		"discovery loads the candidate set from the per-group endpoint")
	must("/api/worktrees/cleanup", "submit posts to the cleanup endpoint")
	must("delete_branches: branchesCb.checked",
		"the delete-branches toggle is sent to the backend")
}
