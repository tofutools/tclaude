package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's global delete-retired command loads every retired agent,
// then hands the exact roster to the keyed transaction island. The Preact owner
// preserves the title/age filters, visible-and-checked selection contract,
// editable failure recovery, and stable per-conv result phase. Node component
// tests pin behavior; this guard pins exclusive ownership and production wiring.
func TestDashboardTransactionDeleteRetiredExclusiveOwnership(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	html := read("dashboard.html")
	island := read("js/transaction-dialog-island.js")
	actions := read("js/transaction-dialog-actions.js")
	controller := read("js/transaction-dialog-controller.js")
	refresh := read("js/refresh.js")
	palette := read("js/palette.js")
	modalMessage := read("js/modal-message.js")

	if strings.Contains(html, `id="delete-retired-modal"`) {
		t.Error("static dashboard HTML still owns #delete-retired-modal")
	}
	for _, required := range []string{
		`kind === 'delete-retired-preview'`, `id="delete-retired-modal"`,
		`id="delete-retired-search"`, `id="delete-retired-age"`,
		`id="delete-retired-select-all"`, `id="delete-retired-select-none"`,
		`id="delete-retired-wt"`, `id="delete-retired-list"`,
		`submitID="delete-retired-submit"`,
		`primaryClass=${result ? 'primary' : 'primary danger'}`,
		"visibleCandidates.filter(",
		"selected.has(candidate.conv_id)",
		"candidate.agent_id || candidate.conv_id",
		"deleteRetiredAgeDays(candidate) >= minAgeDays",
		"await actions.deleteRetiredPreview(request)",
		"await actions.finishDeleteRetired({ kind: descriptor.kind, response: result })",
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island is missing delete-retired contract %q", required)
		}
	}
	for _, required := range []string{
		"export function normalizeDeleteRetiredCandidates(candidates)",
		"result.sort((a, b) => {",
		"kind: 'delete-retired-preview'",
		"candidates: normalizeDeleteRetiredCandidates(candidates)",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("transaction controller is missing delete-retired launch contract %q", required)
		}
	}
	for _, required := range []string{
		"async deleteRetiredPreview({ agents, deleteWorktrees })",
		"'/api/cleanup/agents'",
		"agents, mode: 'delete', delete_worktrees: !!deleteWorktrees",
		"async finishDeleteRetired(result)",
		"try { await refresh(); } finally { state.finish(result); }",
	} {
		if !strings.Contains(actions, required) {
			t.Errorf("transaction actions are missing delete-retired wire contract %q", required)
		}
	}
	for _, required := range []string{
		"async function openDeleteRetiredPreview()",
		"retired = await fetchListFull('retired')",
		"return openDeleteRetiredPreviewDialog(retired)",
		"from './transaction-dialog-controller.js';",
	} {
		if !strings.Contains(refresh, required) {
			t.Errorf("refresh launcher is missing delete-retired cutover %q", required)
		}
	}
	if strings.Contains(refresh, "$('#delete-retired-modal')") {
		t.Error("refresh.js retains the superseded imperative delete-retired owner")
	}
	if !strings.Contains(palette, "run: () => openDeleteRetiredPreview()") {
		t.Error("the command palette lost its delete-retired launcher")
	}
	if !strings.Contains(modalMessage,
		"$('#delete-retired-open').addEventListener('click', () => openDeleteRetiredPreview())") {
		t.Error("the Groups menu lost its delete-retired launcher")
	}

	// Worktree cleanup remains an adjacent owner under its dedicated Preact root.
	// General cleanup and delete-group live in the transaction root.
	for _, adjacent := range []string{
		`id="worktree-cleanup-root"`,
	} {
		if !strings.Contains(html, adjacent) {
			t.Errorf("adjacent static workflow changed during delete-retired cutover: %q", adjacent)
		}
	}
	for _, adjacent := range []string{
		"function openDeleteGroupModal(group)",
		"export async function openCleanupModal(options = {})",
		"function openWorktreeCleanup(group = '')",
	} {
		if !strings.Contains(refresh, adjacent) {
			t.Errorf("adjacent imperative owner changed during delete-retired cutover: %q", adjacent)
		}
	}

	// The palette remains gated by the cheap total rather than fetching a full
	// list merely to render a command.
	for _, required := range []string{
		"const retiredCount = snap.retired_total || 0",
		"if (retiredCount) {",
		"icon: wiz('🗑', '🔥'), label: wiz('Delete retired agents…', 'Dispel banished familiars…')",
		"keywords: 'delete purge retired cleanup remove wipe agents'",
	} {
		if !strings.Contains(palette, required) {
			t.Errorf("delete-retired palette contract missing %q", required)
		}
	}
}
