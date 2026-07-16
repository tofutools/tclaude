package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// Node component tests pin the complete window-selection behavior. This guard
// pins the production cutover: one keyed Preact owner, a snapshot-only launcher,
// and plain adapters for native HTTP plus terminal-shell side effects.
func TestDashboardWindowSelectionExclusiveOwnership(t *testing.T) {
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
	operations := read("js/dashboard-operations.js")
	dashboard := read("js/dashboard.js")

	if strings.Contains(html, `id="window-modal"`) {
		t.Error("static dashboard HTML still owns #window-modal")
	}
	for _, required := range []string{
		`kind === 'window-selection'`, `id="window-modal"`, `id="window-direction"`,
		`id="window-search"`, `id="window-select-all"`, `id="window-select-none"`,
		`id="window-groups"`, `id="window-roles"`, `id="window-list"`,
		`submitID="window-submit"`, `selectedCandidates.map(`,
		`candidate.agent_id || candidate.conv_id`,
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island missing window-selection contract %q", required)
		}
	}
	for _, required := range []string{
		"buildWindowSelectionDescriptor(", "normalizeWindowSelectionCandidates(",
		"kind: 'window-selection'", "openWindowSelectionDialog(descriptor)",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("transaction controller missing window launch contract %q", required)
		}
	}
	for _, required := range []string{
		"async selectAgentWindows(request)", "fetchImpl('/api/agent-windows'",
		"body: JSON.stringify(payload)", "openWebWindowPane(target.selector, target.label)",
		"closeTerminalsForWindowOp(result.agents)",
	} {
		if !strings.Contains(actions, required) {
			t.Errorf("transaction actions missing window adapter contract %q", required)
		}
	}
	for _, required := range []string{
		"function openWindowModal(scope, groupName)",
		"buildWindowSelectionDescriptor(", "openWindowSelectionDialog(descriptor)",
	} {
		if !strings.Contains(operations, required) {
			t.Errorf("operation launcher missing window cutover %q", required)
		}
	}
	if strings.Contains(refresh, "$('#window-modal')") ||
		strings.Contains(refresh, "fetch('/api/agent-windows'") {
		t.Error("refresh.js retains the superseded window dialog or transport owner")
	}
	for _, required := range []string{
		"openWebWindowPane,", "closeTerminalsForWindowOp,",
	} {
		if !strings.Contains(dashboard, required) {
			t.Errorf("dashboard mount missing terminal adapter injection %q", required)
		}
	}
}
