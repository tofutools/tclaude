package agentd

import (
	"strings"
	"testing"
)

// The spawn modal's "Sync worktree branch with alias" checkbox lives
// entirely in dashboard.html's embedded JS — there's no server code
// path to exercise with a flow test. This guards the markup/JS against
// being silently dropped in a future refactor of that file: it asserts
// the checkbox exists and defaults on, the sync helpers are present,
// and the events that drive (and detach) the sync are actually wired.
func TestDashboardHTML_WorktreeAliasSyncWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardHTML, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// The checkbox exists and is auto-checked.
	must(`<input id="agent-spawn-wt-sync" type="checkbox" checked />`,
		"alias-sync checkbox renders, default-on")

	// Sync helpers.
	must("function applyWtSync(", "checkbox -> worktree picker bridge")
	must("function spawnWtLoad(", "picker reload that re-applies the sync")

	// The sync only works against a usable git repo — the checkbox is
	// disabled to match the worktree <select>.
	must("syncEl.disabled = !usable;", "checkbox gated on a valid CWD/repo")

	// Editing the alias mirrors into the worktree branch; toggling the
	// checkbox re-applies the sync.
	must("$('#agent-spawn-alias').addEventListener('input', applyWtSync);",
		"alias edits drive the sync")
	must("$('#agent-spawn-wt-sync').addEventListener('change', applyWtSync);",
		"toggling the checkbox re-applies the sync")

	// Hand-editing the branch or picking a worktree by hand detaches
	// the sync so it stops clobbering the human's choice.
	must("$('#agent-spawn-wt-branch').addEventListener('input', () => {",
		"manual branch edit turns the sync off")
	must("if (e.target.value !== WT_NEW) $('#agent-spawn-wt-sync').checked = false;",
		"manual worktree choice turns the sync off")

	// Every spawn-side picker reload goes through spawnWtLoad so the
	// checkbox state stays consistent — the only wtLoad('agent-spawn')
	// left is the one inside spawnWtLoad itself.
	if n := strings.Count(dashboardHTML, "wtLoad('agent-spawn'"); n != 1 {
		t.Errorf("dashboard.html: want exactly 1 wtLoad('agent-spawn', ...) (inside spawnWtLoad); got %d — other spawn reloads must go through spawnWtLoad", n)
	}
}
