package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardTransactionShutdownExclusiveOwnership(t *testing.T) {
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
	controller := read("js/transaction-dialog-controller.js")
	rowActions := read("js/row-actions.js")
	palette := read("js/palette.js")
	refresh := read("js/refresh.js")
	processes := read("js/processes-actions.js")

	if strings.Contains(html, `id="shutdown-modal"`) {
		t.Error("static dashboard HTML still owns #shutdown-modal")
	}
	if strings.Contains(css, "#shutdown-modal") {
		t.Error("dashboard CSS retains a static #shutdown-modal ownership hook")
	}
	for _, required := range []string{
		`id="shutdown-modal"`, `kind === 'shutdown-agent'`,
		`primaryLabel=${retrying && !forceChoice ? 'Retry soft exit' : 'Soft exit'}`,
		`alternateLabel=${retrying && forceChoice ? 'Retry force kill' : 'Force kill'}`,
	} {
		if !strings.Contains(island, required) {
			t.Errorf("transaction island is missing shutdown ownership contract %q", required)
		}
	}
	if !strings.Contains(controller, "openShutdownAgentDialog(agent, label") {
		t.Error("transaction controller is missing the stable-agent shutdown launcher")
	}
	for name, source := range map[string]string{
		"row actions": rowActions,
		"palette":     palette,
	} {
		if !strings.Contains(source, "openShutdownAgentDialog") {
			t.Errorf("%s does not route shutdown through the transaction controller", name)
		}
		for _, legacy := range []string{"shutdownConfirm", "stopAgentReq"} {
			if strings.Contains(source, legacy) {
				t.Errorf("%s retains legacy shutdown owner %q", name, legacy)
			}
		}
	}
	if !strings.Contains(rowActions, "openShutdownAgentDialog(agent, label)") {
		t.Error("online status-dot does not launch with the stable agent selector")
	}
	if !strings.Contains(palette, "openShutdownAgentDialog(sel, label)") {
		t.Error("palette stop does not launch with agent_id || conv_id selector")
	}
	for _, legacy := range []string{
		"function shutdownConfirm(", "function stopAgentReq(", "$('#shutdown-modal')",
	} {
		if strings.Contains(refresh, legacy) {
			t.Errorf("refresh.js retains superseded shutdown owner %q", legacy)
		}
	}
	// The Processes scribe flow is deliberately separate: it preserves its
	// process-specific confirmation/notices and never offers force-stop.
	for _, preserved := range []string{
		"async function stopScribe(scribe)",
		"JSON.stringify({ force: false })",
		"Could not stop ${scribe.name}",
	} {
		if !strings.Contains(processes, preserved) {
			t.Errorf("Processes stopScribe contract changed or disappeared: %q", preserved)
		}
	}
}
