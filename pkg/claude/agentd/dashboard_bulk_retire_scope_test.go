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
		"await actions.retireAgentsPreview(request)",
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island is missing global-retire contract %q", required)
		}
	}
}
