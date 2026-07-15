package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The command palette's per-group "Retire idle/offline agents in
// <group>" command opens a keyed transaction PREVIEW rather than firing a
// status-filtered bulk retire the server re-resolves from live state. The
// Preact owner lists precisely the matching members, lets the human opt agents
// out, and POSTs the EXPLICIT ticked canonical conv-id list to
// /api/groups/{name}/retire {convs:[…]} — so the BE retires exactly what
// the human previewed.
//
// Node component tests pin behavior; this structural guard pins exclusive
// ownership and the launcher/action wiring across embedded HTML + JS. The
// explicit-convs backend path has its own flow tests in groups_retire_flow_test.go.
func TestDashboardTransactionGroupRetireExclusiveOwnership(t *testing.T) {
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

	if strings.Contains(html, `id="retire-preview-modal"`) {
		t.Error("static dashboard HTML still owns #retire-preview-modal")
	}
	for _, required := range []string{
		`kind === 'retire-group-preview'`, `id="retire-preview-modal"`,
		`id="retire-preview-search"`, `id="retire-preview-select-all"`,
		`id="retire-preview-select-none"`, `id="retire-preview-shutdown"`,
		`id="retire-preview-wt"`, `id="retire-preview-list"`,
		`id="retire-preview-submit"`,
		`<${Words} plain=${regularTitle} wizard=${wizardTitle} />`,
		`convs: selectedCandidates.map((candidate) => candidate.conv_id)`,
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island is missing group-retire contract %q", required)
		}
	}
	for _, required := range []string{
		"openGroupRetirePreviewDialog(group, status, candidates)",
		"dedupeRetireCandidates(candidates)",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("transaction controller is missing group-retire launch contract %q", required)
		}
	}
	for _, required := range []string{
		"async retireGroupPreview({ group, convs, shutdown, deleteWorktree })",
		"`/api/groups/${encodeURIComponent(group)}/retire`",
		"JSON.stringify({ convs, shutdown: !!shutdown, delete_worktree: !!deleteWorktree })",
	} {
		if !strings.Contains(actions, required) {
			t.Errorf("transaction actions are missing group-retire wire contract %q", required)
		}
	}
	for _, required := range []string{
		"function groupMembersByStatus(",
		"openGroupRetirePreviewDialog(group, status, candidates)",
	} {
		if !strings.Contains(refresh, required) {
			t.Errorf("refresh launcher is missing group-retire cutover %q", required)
		}
	}
	if strings.Contains(refresh, "$('#retire-preview-modal')") {
		t.Error("refresh.js retains the superseded imperative retire-preview owner")
	}
	if !strings.Contains(palette, "openRetirePreview(g.name, status)") {
		t.Error("the per-group palette command lost its preview launcher")
	}
	for _, adjacent := range []string{
		`id="delete-retired-modal"`, `id="delete-group-modal"`, `id="worktree-cleanup-modal"`,
	} {
		if !strings.Contains(html, adjacent) {
			t.Errorf("adjacent static workflow changed during retire cutover: %q", adjacent)
		}
	}
}
