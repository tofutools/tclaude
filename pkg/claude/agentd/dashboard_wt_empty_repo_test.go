package agentd

import (
	"strings"
	"testing"
)

// The worktree picker's empty-repo (unborn-HEAD) handling lives in the
// shared picker JS (modal-link-wt.js) + the spawn/clone/template modal markup —
// no server path renders it, so this guards the wiring against a silent
// drop in a future refactor. The actual orphan-worktree creation is
// covered end-to-end by TestSpawnCLI_WorktreeInEmptyRepoCutsOrphan and
// worktree.TestAddWorktreeInEmptyRepo.
func TestDashboardHTML_EmptyRepoOrphanHintWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// All worktree-bearing modals carry the orphan warning the picker
	// reveals when the repo has no commits, styled as the amber callout.
	must(`id="agent-spawn-wt-orphan-hint"`, "spawn modal orphan warning exists")
	must(`id="clone-agent-wt-orphan-hint"`, "clone modal orphan warning exists")
	must(`id="template-deploy-wt-orphan-hint"`, "template deploy modal orphan warning exists")
	must(`class="wt-orphan-warn"`, "orphan warning uses the callout class")
	must(".wt-orphan-warn {", "orphan warning callout is styled")

	// The picker stamps the no-commits flag from the API response and
	// reads it back when toggling the "+ create" rows.
	must("select.dataset.hasCommits = data.has_commits === false ? '0' : '1';",
		"picker stamps has_commits from the /api/worktrees response")
	must("$(`#${prefix}-worktree`).dataset.hasCommits === '0'",
		"wtToggleNew reads the no-commits flag")
	must("`${prefix}-wt-orphan-hint`",
		"wtToggleNew toggles the orphan hint element")
}
