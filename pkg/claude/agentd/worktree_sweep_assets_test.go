package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// Component/model/action tests pin the worktree janitor's behavior. This guard
// pins the production ownership boundary across embedded assets so static DOM
// or refresh.js cannot silently regain an imperative writer.
func TestDashboardWorktreeCleanupPreactOwnership(t *testing.T) {
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
	loader := read("js/preact-loader.js")
	refresh := read("js/refresh.js")
	operations := read("js/dashboard-operations.js")
	controller := read("js/worktree-cleanup-controller.js")
	model := read("js/worktree-cleanup-model.js")
	actions := read("js/worktree-cleanup-actions.js")
	island := read("js/worktree-cleanup-island.js")

	if !strings.Contains(html, `id="worktree-cleanup-root"`) {
		t.Error("dashboard HTML is missing the stable worktree-cleanup Preact host")
	}
	if strings.Contains(html, `id="worktree-cleanup-modal"`) {
		t.Error("static dashboard HTML still owns #worktree-cleanup-modal")
	}
	for _, required := range []string{
		"mountWorktreeCleanupFeature({", "refresh: dashboardActions.refresh", "notify: toast",
	} {
		if !strings.Contains(dashboard, required) {
			t.Errorf("dashboard bootstrap is missing worktree owner wiring %q", required)
		}
	}
	for _, required := range []string{
		"name: 'worktree-cleanup'", "hosts: { root: '#worktree-cleanup-root' }",
		"createWorktreeCleanupState", "createWorktreeCleanupActions", "mountWorktreeCleanupIsland",
	} {
		if !strings.Contains(loader, required) {
			t.Errorf("Preact loader is missing worktree owner wiring %q", required)
		}
	}
	for _, required := range []string{
		"registerWorktreeCleanupController", "export function openWorktreeCleanup(group = '')",
		"return requireController().open(group)",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("worktree controller is missing launch contract %q", required)
		}
	}
	for _, required := range []string{
		"function openWorktreeCleanup(group = '')", "openWorktreeCleanupDialog(group)",
	} {
		if !strings.Contains(operations, required) {
			t.Errorf("operation launcher is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"$('#worktree-cleanup-modal')", "$('#worktree-cleanup-list')",
		"worktree-cleanup-submit').addEventListener", "async function load(isRescan)",
	} {
		if strings.Contains(refresh, forbidden) {
			t.Errorf("refresh.js retains superseded worktree ownership %q", forbidden)
		}
	}

	for _, required := range []string{
		`id="worktree-cleanup-modal"`, `id="worktree-cleanup-list"`,
		`id="worktree-cleanup-search"`, `id="worktree-cleanup-select-all"`,
		`id="worktree-cleanup-select-none"`, `id="worktree-cleanup-rescan"`,
		`id="worktree-cleanup-categories"`, `id="worktree-cleanup-branches"`,
		`id="worktree-cleanup-submit"`, `id="worktree-cleanup-cancel"`,
		"reconcileWorktreeCandidates(response.worktrees, touchedChoices.current)",
		"const request = submittedRequest || freezeWorktreeCleanupRequest(",
		"setResult(response)", "CleanupOutcomeList", "state.finish(result ? { response: result } : null)",
	} {
		if !strings.Contains(island, required) {
			t.Errorf("worktree island is missing controlled UI contract %q", required)
		}
	}
	for _, required := range []string{
		"candidate.is_main || !touchedChoices.has(candidate.path)",
		"checked: !isMain && worktree?.checked === true",
		"paths: Object.freeze(selectedWorktrees(candidates).map(",
	} {
		if !strings.Contains(model, required) {
			t.Errorf("worktree model is missing selection/safety contract %q", required)
		}
	}
	for _, required := range []string{
		"`/api/groups/${encodeURIComponent(normalizedGroup)}/worktrees`",
		"'/api/worktrees/cleanup'", "paths: request.paths",
		"delete_branches: request.deleteBranches === true", "outcomes: Object.freeze(",
	} {
		if !strings.Contains(actions, required) {
			t.Errorf("worktree actions are missing scan/cleanup wire contract %q", required)
		}
	}

	// Existing group/global entry points continue to route through the same
	// compatibility launcher after ownership moves.
	for _, required := range []string{
		"openWorktreeCleanup(group)", "run: () => openWorktreeCleanup()",
		`id="cleanup-worktrees-all"`, `data-act="cleanup-worktrees-group"`,
		"case 'cleanup-worktrees-group'",
	} {
		if !strings.Contains(dashboardAssets, required) {
			t.Errorf("worktree entry point is missing %q", required)
		}
	}
}
