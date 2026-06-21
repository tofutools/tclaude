package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_SpawnRemoteControlWired guards the JOH-258 spawn-form
// option: a "Start with remote control" checkbox that is gated on the chosen
// harness having built-in Remote Access (Claude Code's can_remote_control —
// hidden for Codex), defaults off on open, and rides into the spawn POST body
// as `remote_control`. It spans dashboard.html (the row) + modal-spawn.js (the
// gating + body assembly); the repo has no JS test runner, so this asserts on
// the embedded concatenation at `go test ./...`.
func TestDashboardHTML_SpawnRemoteControlWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// dashboard.html: the checkbox + its row exist.
	must(`id="agent-spawn-remote-control-row"`, "spawn dialog has a remote-control row")
	must(`id="agent-spawn-remote-control"`, "spawn dialog has a remote-control checkbox")

	// modal-spawn.js: the row is gated on the harness capability (shown for a
	// harness with Remote Access, hidden + cleared for one without it).
	must("h.can_remote_control", "the remote-control row gates on the harness capability")
	must("#agent-spawn-remote-control-row", "applySpawnHarness toggles the remote-control row")

	// modal-spawn.js: it defaults OFF on open (a deliberate per-spawn choice,
	// not a sticky pref) and the body carries `remote_control` only when ticked
	// AND the harness supports it.
	must("$('#agent-spawn-remote-control').checked = false", "the checkbox defaults off on open")
	must("body.remote_control = true", "the spawn body carries remote_control when opted in")
	must("harnessEntry.can_remote_control && $('#agent-spawn-remote-control').checked",
		"the body opt-in is gated on both the harness capability and the checkbox")
}
