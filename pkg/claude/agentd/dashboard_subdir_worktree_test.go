package agentd

import (
	"strings"
	"testing"
)

// The spawn modal's "worktree a sub-repo of a monorepo launch dir"
// wiring lives entirely in dashboard.html's embedded JS — no server
// path exercises it, so this guards the markup/JS against a silent
// drop in a future refactor (same role as the sortable-columns test).
func TestDashboardHTML_SubdirWorktreeWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardHTML, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// The "Worktree repo" field, decoupled from CWD, plus its
	// sub-repo datalist for monorepo drill-down.
	must(`id="agent-spawn-wt-repo"`, "worktree-repo input exists")
	must(`id="agent-spawn-subrepo-list"`, "sub-repo datalist exists")
	must(`list="agent-spawn-subrepo-list"`, "input is bound to the datalist")

	// The picker helpers the field drives.
	must("function wtResolve(", "selection resolver ({path,branch})")
	must("function wtFillSubRepos(", "sub-repo datalist populator")

	// Submit threads the worktree through as separate fields when the
	// worktree repo differs from CWD (the monorepo case).
	must("worktree_path", "submit sends worktree_path")
	must("worktree_branch", "submit sends worktree_branch")
	must("spawnWtRepoEdited", "CWD-mirror detach flag")
}

// The CWD / Branch columns stack an init/now pair when an agent has
// moved off its launch dir. That rendering lives in dashboard.html's
// JS; this guards the helpers + the startup/current fields they read.
func TestDashboardHTML_LocationCellsWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardHTML, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// The stacked-cell helpers.
	must("function stackedLoc(", "init/now stacking helper")
	must("function cwdCell(", "CWD column cell renderer")
	must("function branchCell(", "Branch column cell renderer")
	must("loc-pair", "stacked-pair CSS class")

	// The cells read the startup/current split off the snapshot rows.
	must("startup_dir", "cwdCell reads startup_dir")
	must("current_dir", "cwdCell reads current_dir")
	must("startup_branch", "branchCell reads startup_branch")
}
