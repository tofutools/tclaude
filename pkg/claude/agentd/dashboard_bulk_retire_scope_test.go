package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// Status-filtered palette retirement has three explicit scopes: a real group,
// the virtual Ungrouped group, and the distinct global Agents roster. The
// latter includes Ungrouped without double-counting multi-group agents.
func TestDashboardBulkRetireScopeExpansion(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	palette := read("js/palette.js")
	operations := read("js/dashboard-operations.js")
	controller := read("js/transaction-dialog-controller.js")
	island := read("js/transaction-dialog-island.js")
	css := read("dashboard.css")

	for _, required := range []string{
		"const allCount = new Set((snap.agents || [])",
		"const ungroupedN = new Set((snap.ungrouped || [])",
		"openRetireAllPreview(status)",
		"openRetireUngroupedPreview(status)",
		"`Retire ${status} agents across all groups`",
		"`Banish ${status} familiars across all parties`",
		"`Retire ${status} agents in Ungrouped`",
		"`Banish ${status} familiars in Unbound`",
		"including Ungrouped",
		"including the Unbound",
	} {
		if !strings.Contains(palette, required) {
			t.Errorf("palette is missing bulk-retire scope contract %q", required)
		}
	}
	for _, required := range []string{
		"function retireCandidatesByStatus(rows, status = '')",
		"return retireCandidatesByStatus((lastSnapshot || {}).agents, status);",
		"return retireCandidatesByStatus((lastSnapshot || {}).ungrouped, status);",
		"groups: [...new Set((Array.isArray(a.groups) ? a.groups : [])",
		"export function openRetireAllPreview(status)",
		"openAllRetirePreviewDialog(status, candidates)",
	} {
		if !strings.Contains(operations, required) {
			t.Errorf("operations are missing bulk-retire scope contract %q", required)
		}
	}
	for _, required := range []string{
		"export function openAllRetirePreviewDialog(status, candidates)",
		"kind: 'retire-all-preview'",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("controller is missing global-retire contract %q", required)
		}
	}
	for _, required := range []string{
		`descriptor.kind === 'retire-all-preview'`,
		"`Retire ${descriptor.status} agents across all groups`",
		"`Banish ${descriptor.status} familiars across all parties`",
		"including Ungrouped",
		"including the Unbound",
		"descriptor.kind === 'retire-all-preview' ? html",
		"candidate.groups?.length ? `in: ${candidate.groups.join(', ')}` : 'in: Ungrouped'",
		"candidate.groups?.length ? `parties: ${candidate.groups.join(', ')}` : 'in: Unbound'",
		"await actions.retireAgentsPreview(request)",
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island is missing global-retire contract %q", required)
		}
	}
	for _, required := range []string{
		"#retire-preview-modal .cleanup-row .retire-preview-groups",
		"min-width: 0; overflow: visible; white-space: normal;",
		"text-overflow: clip; overflow-wrap: anywhere;",
	} {
		if !strings.Contains(css, required) {
			t.Errorf("dashboard CSS is missing non-eliding group membership contract %q", required)
		}
	}
	for _, required := range []string{
		"body.wizard #retire-preview-modal .cleanup-modal {",
		"body.wizard #retire-preview-modal .cleanup-toolbar input[type=search] {",
		"body.wizard #retire-preview-modal .cleanup-list {",
		"body.wizard #retire-preview-modal .cleanup-row label .cleanup-badge {",
		"body.wizard #retire-preview-modal .delete-agent-wt input[type=checkbox] {",
		"body.wizard #retire-preview-modal #retire-preview-submit {",
		"body.wizard #retire-preview-modal #retire-preview-submit.danger {",
	} {
		if !strings.Contains(css, required) {
			t.Errorf("dashboard CSS is missing wizard bulk-retire styling contract %q", required)
		}
	}
}
