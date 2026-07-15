package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardSmallDialogsPreactOwnership(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	html := read("dashboard.html")
	components := read("js/small-dialog-components.js")
	actions := read("js/action-dialog-actions.js")
	state := read("js/action-dialog-state.js")
	controller := read("js/action-dialog-controller.js")
	rowActions := read("js/row-actions.js")
	refresh := read("js/refresh.js")
	dashboard := read("js/dashboard.js")

	if !strings.Contains(html, `id="action-dialog-root"`) {
		t.Fatal("dashboard is missing the stable Preact action-dialog host")
	}
	for _, retired := range []string{
		`id="clone-modal"`, `id="export-agent-modal"`, `id="term-modal"`,
	} {
		if strings.Contains(html, retired) {
			t.Errorf("static dashboard markup still owns migrated dialog %s", retired)
		}
	}
	for _, kept := range []string{`id="terminal-session-root"`} {
		if !strings.Contains(html, kept) {
			t.Errorf("explicit xterm exclusion was removed: %s", kept)
		}
	}

	for _, path := range []string{"js/modal-clone.js", "js/modal-export.js"} {
		if _, err := fs.ReadFile(dashboardAssetsFS, path); err == nil {
			t.Errorf("retired imperative controller still embedded: %s", path)
		}
	}
	for _, forbidden := range []string{"fetch(", "innerHTML", "addEventListener", "./refresh.js", "modal-term", "xterm"} {
		if strings.Contains(components, forbidden) {
			t.Errorf("Preact small-dialog owner crosses its presentation boundary with %q", forbidden)
		}
	}
	for _, forbidden := range []string{"document", "querySelector", "innerHTML"} {
		if strings.Contains(actions+state, forbidden) {
			t.Errorf("plain small-dialog action/state boundary contains DOM knowledge %q", forbidden)
		}
	}
	for _, required := range []string{
		"openPresetCloneDialog", "openAgentExportDialog", "chooseTerminalDirectory",
	} {
		if !strings.Contains(controller, required) {
			t.Errorf("compatibility controller missing %q", required)
		}
	}
	for _, required := range []string{
		"openAgentExportDialog(agent, label)",
		"chooseTerminalDirectory(label)",
	} {
		if !strings.Contains(rowActions, required) {
			t.Errorf("row action does not route through Preact controller: %q", required)
		}
	}
	if strings.Contains(refresh, "function termDirModal(") {
		t.Error("refresh.js still owns the terminal-directory chooser UI")
	}
	for _, retired := range []string{"bindCloneModal", "bindExportModal", "modal-clone.js", "modal-export.js"} {
		if strings.Contains(dashboard, retired) {
			t.Errorf("dashboard boot still binds retired imperative UI %q", retired)
		}
	}
	for _, cleanup := range []string{
		"controller?.abort()", "clearTimer(timer)", "return () => {", "state.dispose()",
	} {
		if !strings.Contains(actions+components+read("js/action-dialog-island.js"), cleanup) {
			t.Errorf("small-dialog lifecycle is missing cleanup contract %q", cleanup)
		}
	}
}
