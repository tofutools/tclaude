package agentd

import (
	"strings"
	"testing"
)

// The spawn island's "Sync worktree branch with name" checkbox has no server
// path, so pin the controlled field and its pure model transitions.
func TestDashboardHTML_WorktreeNameSyncWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// The checkbox exists and is auto-checked.
	must(`<input id="agent-spawn-wt-sync" type="checkbox" checked=${draft.syncWorktree}`,
		"name-sync checkbox renders, default-on")

	// The default lives in the draft and all synchronization is pure.
	must("syncWorktree: true", "draft defaults synchronization on")
	must("export function syncSpawnWorktree(", "checkbox -> worktree picker bridge")

	// The sync only works against a usable git repo — the checkbox is
	// disabled to match the worktree <select>.
	must("disabled=${busy || !worktreeUsable}", "checkbox gated on a valid CWD/repo")

	// Editing the name mirrors into the worktree branch; toggling the
	// checkbox re-applies the sync.
	must("syncSpawnWorktree({ ...before, name: value }, worktrees.isRepo)",
		"name edits drive the sync")
	must("syncSpawnWorktree({ ...before, syncWorktree: event.currentTarget.checked }, worktreeUsable)",
		"toggling the checkbox re-applies the sync")

	// Hand-editing the branch or picking a worktree by hand detaches
	// the sync so it stops clobbering the human's choice.
	must("worktreeBranch: event.currentTarget.value, syncWorktree: false",
		"manual branch edit turns the sync off")
	must("syncWorktree: value === WT_NEW ? draft.syncWorktree : false",
		"manual worktree choice turns the sync off")
}
