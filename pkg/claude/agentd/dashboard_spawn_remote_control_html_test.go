package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_SpawnRemoteControlWired guards the JOH-258 spawn-form
// option, with the JOH-262-revised semantics: a "Start with remote control"
// checkbox gated on the chosen harness having built-in Remote Access (Claude
// Code's can_remote_control — hidden for Codex), PRE-FILLED from the group's
// remote-control policy + the picked profile's default, and then authoritative —
// its state (true OR false) always rides into the spawn POST body as
// `remote_control`. It spans dashboard.html (the row) + modal-spawn.js (the
// gating + prefill + body assembly); the repo has no JS test runner, so this
// asserts on the embedded concatenation at `go test ./...`.
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

	// modal-spawn.js: the checkbox is PRE-FILLED from the group's remote-control
	// policy (optin/deny), falling back to the picked profile's default, then off.
	must("function groupRemoteControlPolicy(", "a helper reads the group's remote-control policy")
	must("g.remote_control_policy", "the prefill reads the group's remote_control_policy")
	must("function applyRemoteControlPrefill(", "a helper pre-fills the checkbox from the spawn defaults")
	must("policy === 'optin'", "the prefill maps the group optin policy to a checked box")
	must("applyRemoteControlPrefill(select.value, null)", "the modal pre-fills the checkbox on open")
	must("applyRemoteControlPrefill($('#agent-spawn-group').value, p)",
		"applying a profile re-derives the checkbox from group policy + profile")
	// The checkbox must re-derive when the group is switched mid-dialog (else it
	// stays on the prior group's policy and the authoritative submit is wrong).
	must("applyRemoteControlPrefill(e.target.value, null)",
		"switching the group re-derives the checkbox from the new group's policy")

	// modal-spawn.js: the checkbox is authoritative — the body carries its state
	// (true OR false) whenever the harness supports Remote Access, so an explicit
	// uncheck of an optin-pre-filled box is honoured server-side.
	must("body.remote_control = $('#agent-spawn-remote-control').checked",
		"the spawn body carries the checkbox state explicitly")
	must("harnessEntry && harnessEntry.can_remote_control",
		"the body remote_control is gated on the harness capability")
}
