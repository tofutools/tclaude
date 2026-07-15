package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The command palette's global "Retire ungrouped agents…" command is the
// cross-group cleanup twin of the per-group "Retire idle/offline agents
// in <group>" sweep. Ungrouped agents belong to no group, so there is no
// group retire route to reach them; instead the command opens the SAME
// keyed transaction preview and POSTs the human's explicit ticked list to the
// group-agnostic bulk cleanup endpoint
// (/api/cleanup/agents {mode:"retire"}) with include_online set.
//
// Node component tests pin behavior; this structural guard pins ownership and
// exact launcher/action wiring. The backend retire+worktree path has its own
// flow tests (cleanup_flow_test.go: TestCleanup_Agents_Retire*).
func TestDashboardTransactionUngroupedRetireExclusiveOwnership(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	island := read("js/transaction-dialog-island.js")
	actions := read("js/transaction-dialog-actions.js")
	controller := read("js/transaction-dialog-controller.js")
	refresh := read("js/refresh.js")
	palette := read("js/palette.js")

	// 1. The palette offers the command, gated on a live count so it is
	//    never a no-op, with both the plain label and the 🧙 wizard synonym
	//    ("Banish unbound familiars…"). The plain "retire"/wizard "banish"
	//    words already bridge via the scorer's SYNONYMS map.
	for _, required := range []string{
		"const ungroupedCount = new Set((snap.ungrouped || []).map(a => a.conv_id).filter(Boolean)).size;",
		"if (ungroupedCount) {",
		"wiz('Retire ungrouped agents…', 'Banish unbound familiars…')",
		"run: () => openRetireUngroupedPreview(),",
	} {
		if !strings.Contains(palette, required) {
			t.Errorf("palette is missing ungrouped-retire contract %q", required)
		}
	}
	for _, required := range []string{
		`kind === 'retire-ungrouped-preview'`,
		`agents: Object.freeze(selectedCandidates.map((candidate) => candidate.agent_id || candidate.conv_id))`,
		`: 'Retire ungrouped agents';`, `: 'Banish unbound familiars';`,
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island is missing ungrouped-retire contract %q", required)
		}
	}
	if !strings.Contains(controller, "openUngroupedRetirePreviewDialog(candidates)") {
		t.Error("transaction controller is missing the ungrouped-retire launcher")
	}
	for _, required := range []string{
		"async retireUngroupedPreview({ agents, shutdown, deleteWorktrees })",
		"'/api/cleanup/agents'",
		"mode: 'retire', include_online: true",
		"delete_worktrees: !!deleteWorktrees",
	} {
		if !strings.Contains(actions, required) {
			t.Errorf("transaction actions are missing ungrouped-retire wire contract %q", required)
		}
	}
	for _, required := range []string{
		"function ungroupedRetireCandidates(",
		"for (const a of (snap.ungrouped || [])) {",
		"if (!a.conv_id || seen.has(a.conv_id)) continue;",
		"function countUngroupedAgents(",
		"const candidates = ungroupedRetireCandidates();",
		"openUngroupedRetirePreviewDialog(candidates)",
		"from './transaction-dialog-controller.js';",
	} {
		if !strings.Contains(refresh, required) {
			t.Errorf("refresh launcher is missing ungrouped-retire cutover %q", required)
		}
	}
}
