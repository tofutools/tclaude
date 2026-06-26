package agentd

import (
	"strings"
	"testing"
)

// The spawn modal asks for a name OR an initial description before it will
// go through with a spawn: with both blank the new agent only gets an
// auto-generated label, which is almost always a slip (the human typed an
// initial message and forgot the name). submitAgentSpawn pops the shared
// confirm overlay in that case, and — because the overlay's Esc/Cancel does
// NOT close the underlying spawn modal — a cancel leaves the human back on
// the still-populated form to correct it. This is pure embedded JS with no
// server path of its own, so guard the wiring against a silent drop in a
// future refactor. Mirrors TestDashboardHTML_SpawnNameNormalizeWired.
func TestDashboardHTML_SpawnNameOrDescrRequired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// modal-spawn pulls in the shared confirm overlay for the gate.
	must("confirmModal } from './refresh.js'", "imports the shared confirm overlay")

	// The gate itself: trigger on both-blank, with an explicit "spawn anyway".
	must("if (!name && !descr) {", "gate fires only when name AND description are blank")
	must("title: 'Spawn without a name?'", "the confirm asks before an unnamed spawn")
	must("okLabel: 'Spawn anyway'", "the human can still proceed")
}
