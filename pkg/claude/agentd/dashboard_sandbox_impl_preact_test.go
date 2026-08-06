package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The sandbox-implementation picker is a Preact-owned dialog reached from a row
// action, the same split every other action dialog uses: the row emits an
// intent, the registered controller opens it, and the dialog owns its draft.
// This guard pins that wiring across the embedded assets — a dialog whose
// controller entry is missing fails only at click time, in the browser.
func TestDashboardSandboxImplDialogWiring(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	state := read("js/action-dialog-state.js")
	controller := read("js/action-dialog-controller.js")
	actions := read("js/action-dialog-actions.js")
	island := read("js/action-dialog-island.js")
	components := read("js/small-dialog-components.js")
	table := read("js/groups-member-table.js")
	rows := read("js/row-action-handler.js")
	css := read("dashboard.css")

	for _, probe := range []struct{ file, name, want string }{
		{state, "action-dialog-state.js", `openSandboxImpl({ conv, label = '', harness = '' })`},
		{state, "action-dialog-state.js", `kind: 'sandbox-impl'`},
		{controller, "action-dialog-controller.js", `export function openSandboxImplDialog(`},
		{actions, "action-dialog-actions.js", `openSandboxImpl: state.openSandboxImpl`},
		{actions, "action-dialog-actions.js", `async loadSandboxImpl(`},
		{actions, "action-dialog-actions.js", `async assignSandboxImpl(`},
		{actions, "action-dialog-actions.js", `sandboxImplOptions(harnessName)`},
		{actions, "action-dialog-actions.js", `sandboxModes(harnessName)`},
		{island, "action-dialog-island.js", `descriptor.kind === 'sandbox-impl'`},
		{components, "small-dialog-components.js", `export function SandboxImplDialog(`},
		{components, "small-dialog-components.js", `id="sandbox-impl-modal"`},
		// ManagementOverlay's onClose is the TERMINAL step of its close
		// transaction. Routing it back into requestClose re-enters a guarded
		// close that no-ops, leaving a dialog only a successful POST can dismiss
		// — and, since the descriptor stays owned, blocking every other action
		// dialog behind it. The behaviour is covered in jstest; this pins the
		// shape so the wrong one cannot be reintroduced silently.
		{components, "small-dialog-components.js", `onClose=${() => actions.close(descriptor)}`},
		{components, "small-dialog-components.js", `id="sandbox-impl-options"`},
		{components, "small-dialog-components.js", `id="sandbox-impl-mode"`},
		{components, "small-dialog-components.js", `id="sandbox-impl-assign"`},
		{table, "groups-member-table.js", `function SandboxImplMenuItem(`},
		{table, "groups-member-table.js", `act="sandbox-impl"`},
		{rows, "row-action-handler.js", `case 'sandbox-impl':`},
		{rows, "row-action-handler.js", `openSandboxImplDialog({`},
		{css, "dashboard.css", `#sandbox-impl-options`},
		{css, "dashboard.css", `.sandbox-impl-option.selected`},
	} {
		if !strings.Contains(probe.file, probe.want) {
			t.Errorf("%s: missing %q", probe.name, probe.want)
		}
	}
}

// The action records durable relaunch intent, which the NEXT launch applies —
// the opposite liveness requirement from the restart actions beside it in the
// same menu. Disabling it while the agent runs, with a reason, is what keeps an
// operator from discovering the rule as a 409.
func TestDashboardSandboxImplMenuItemIsOfflineOnly(t *testing.T) {
	data, err := fs.ReadFile(dashboardAssetsFS, "js/groups-member-table.js")
	if err != nil {
		t.Fatalf("read groups-member-table.js: %v", err)
	}
	source := string(data)
	start := strings.Index(source, "function SandboxImplMenuItem(")
	if start < 0 {
		t.Fatal("SandboxImplMenuItem is gone")
	}
	end := strings.Index(source[start:], "\nfunction ")
	if end < 0 {
		t.Fatal("could not bound SandboxImplMenuItem")
	}
	body := source[start : start+end]

	if !strings.Contains(body, "disabled=${!!member.online}") {
		t.Error("the item must be disabled while the agent is ONLINE; " +
			"the restart items beside it disable while OFFLINE, and copying " +
			"that predicate would invert this action")
	}
	if !strings.Contains(body, "member.online") || !strings.Contains(body, "Stop the agent first") {
		t.Error("a disabled item must say why: the tooltip has to name the stop-assign-wake sequence")
	}
	// The dialog resolves the authoritative posture itself. A menu item that
	// passed the row's recorded implementation would be handing the dialog
	// last-launch state as if it were relaunch intent.
	if strings.Contains(body, "sandbox_implementation") {
		t.Error("the menu item must not carry a recorded implementation into the dialog")
	}
}
