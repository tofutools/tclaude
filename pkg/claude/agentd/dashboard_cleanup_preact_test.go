package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The general cleanup launchers collect a complete, click-time roster and
// hand it to the keyed transaction island. Component tests pin the controlled
// form and retry behavior; this guard pins exclusive ownership and production
// wiring across the embedded dashboard assets.
func TestDashboardTransactionCleanupExclusiveOwnership(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	html := read("dashboard.html")
	dashboard := read("js/dashboard.js")
	refresh := read("js/refresh.js")
	controller := read("js/transaction-dialog-controller.js")
	actions := read("js/transaction-dialog-actions.js")
	island := read("js/transaction-dialog-island.js")

	if strings.Contains(html, `id="cleanup-modal"`) {
		t.Error("static dashboard HTML still owns #cleanup-modal")
	}
	if !strings.Contains(html, `id="worktree-cleanup-modal"`) {
		t.Error("the adjacent worktree cleanup owner was removed")
	}
	for _, required := range []string{
		`kind === 'cleanup'`, `function CleanupDialog(`,
		`id="cleanup-modal"`, `id="cleanup-select-all"`, `id="cleanup-select-none"`,
		`id="cleanup-age"`, `id="cleanup-search"`, `id="cleanup-cats"`,
		`id="cleanup-options"`, `id="cleanup-worktrees-all"`,
		`selectedCandidates = candidates.filter(`,
		`rowVisible(candidate) && rowEnabled(candidate)`,
		`targets: Object.freeze(selectedCandidates.map(`,
		`const request = submittedRequest || Object.freeze({`,
		`await actions.cleanup(request)`, `setResult(response || {})`,
		`actions.finishCleanup(result)`,
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island is missing cleanup contract %q", required)
		}
	}
	for _, required := range []string{
		"export function buildCleanupDescriptor(",
		"export function normalizeCleanupCandidates(candidates)",
		"export function openCleanupDialog(descriptor)",
		"kind: 'cleanup'", "if (member?.online) continue",
		"categories: mode === 'group' ? ['agent'] : cleanupCategories(options.categories)",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("transaction controller is missing cleanup launch contract %q", required)
		}
	}
	for _, required := range []string{
		"async cleanup(request)",
		"const url = groupMode ? '/api/cleanup/group' : '/api/cleanup/agents'",
		"members: request.targets", "agents: request.targets",
		"include_owners: !!request.includeOwners",
		"include_online: !!request.includeOnline",
		"delete_worktrees: !!request.deleteWorktrees",
		"shutdown: !!request.shutdown",
		"async finishCleanup(response)",
		"async handoffCleanupWorktrees(descriptor = {})",
	} {
		if !strings.Contains(actions, required) {
			t.Errorf("transaction actions are missing cleanup wire contract %q", required)
		}
	}
	for _, required := range []string{
		"export async function openCleanupModal(options = {})",
		"const snapshot = lastSnapshot",
		"fetchListFull('retired')", "fetchListFull('conversations')",
		"openCleanupDialog(buildCleanupDescriptor(snapshot, options, completeLists))",
	} {
		if !strings.Contains(refresh, required) {
			t.Errorf("refresh launcher is missing cleanup cutover %q", required)
		}
	}
	for _, forbidden := range []string{
		"$('#cleanup-modal')", "$('#cleanup-list')", "$('#cleanup-submit')",
		"cleanup-select-all').addEventListener", "function renderResult(resp)",
	} {
		if strings.Contains(refresh, forbidden) {
			t.Errorf("refresh.js retains superseded cleanup ownership %q", forbidden)
		}
	}
	if strings.Count(dashboard, "openWorktreeCleanup,") < 2 {
		t.Error("transaction island did not receive the worktree follow-up adapter")
	}
}
