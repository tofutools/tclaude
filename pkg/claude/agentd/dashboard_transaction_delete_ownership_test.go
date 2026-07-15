package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardTransactionDeleteExclusiveOwnership(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	html := read("dashboard.html")
	css := read("dashboard.css")
	island := read("js/transaction-dialog-island.js")
	actions := read("js/transaction-dialog-actions.js")
	controller := read("js/transaction-dialog-controller.js")
	memberTable := read("js/groups-member-table.js")
	rowActions := read("js/row-actions.js")
	refresh := read("js/refresh.js")
	palette := read("js/palette.js")
	dnd := read("js/dnd.js")

	if strings.Contains(html, `id="delete-agent-modal"`) {
		t.Error("static dashboard HTML still owns #delete-agent-modal")
	}
	if strings.Contains(css, "#delete-agent-modal") {
		t.Error("dashboard CSS retains a static #delete-agent-modal ownership hook")
	}
	for _, required := range []string{
		`id="delete-agent-modal"`, `kind === 'delete-agent'`,
		`plain="Wipes the conversation history (.jsonl)`,
		`wizard="Burns the conversation scroll (.jsonl)`,
		`plain="shared with another agent" wizard="shared with another familiar"`,
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island is missing delete ownership/copy contract %q", required)
		}
	}
	if !strings.Contains(controller, "openDeleteAgentDialog(agent, label") {
		t.Error("transaction controller is missing stable-agent permanent delete launcher")
	}
	for _, required := range []string{
		"async deleteAgent({ agent, label, deleteWorktree, expectedWorktree })",
		"deleteWorktree === true",
		"params.set('delete_worktree', '1')",
		"params.set('expected_worktree', choice.expectedWorktree)",
	} {
		if !strings.Contains(actions, required) {
			t.Errorf("delete transaction action is missing %q", required)
		}
	}
	if !strings.Contains(island, "expectedWorktree: worktree.path") {
		t.Error("delete transaction does not freeze the probed worktree path")
	}
	if !strings.Contains(memberTable,
		`member=${member} act="delete-agent" className="danger" regular="delete" wizard="erase familiar"`) {
		t.Error("ungrouped member menu lost its permanent-delete launcher/copy")
	}
	if strings.Count(memberTable, `act="delete-agent"`) != 1 {
		t.Error("permanent-delete menu launcher must remain ungrouped-only")
	}
	if !strings.Contains(rowActions, "await openDeleteAgentDialog(agent, label)") {
		t.Error("delegated permanent delete does not pass the stable agent selector")
	}
	if strings.Contains(rowActions, "deleteAgentModal") {
		t.Error("row actions retains the imperative permanent-delete owner")
	}
	for _, legacy := range []string{
		"function deleteAgentModal(", "$('#delete-agent-modal')", "$('#delete-agent-wt-row')",
	} {
		if strings.Contains(refresh, legacy) {
			t.Errorf("refresh.js retains superseded delete owner %q", legacy)
		}
	}
	// Permanent delete remains intentionally absent from palette and DnD. DnD
	// trash continues to retire agents and delete pending spawns only.
	for name, source := range map[string]string{"palette": palette, "DnD": dnd} {
		if strings.Contains(source, "openDeleteAgentDialog") {
			t.Errorf("%s gained an out-of-scope permanent-delete launcher", name)
		}
	}
	// Adjacent destructive flows retain their separate contracts.
	for source, required := range map[string]string{
		rowActions: "case 'delete-generation'",
		refresh:    "function openDeleteGroupModal(group)",
		dnd:        "openRetireAgentDialog",
		html:       `id="delete-group-modal"`,
	} {
		if !strings.Contains(source, required) {
			t.Errorf("adjacent delete/retire contract disappeared: %q", required)
		}
	}
	if !strings.Contains(css, ".delete-agent-wt {") {
		t.Error("shared worktree-choice styling was removed with the static owner")
	}
}
