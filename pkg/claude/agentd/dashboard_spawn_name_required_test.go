package agentd

import (
	"strings"
	"testing"
)

// The spawn modal HARD-requires a name OR an initial description: with both
// blank the new agent would only get an auto-generated label, which is almost
// always a slip (the human typed an initial message and forgot the name).
// submitAgentSpawn rejects that outright with an inline error and re-focuses
// the Name field — it does NOT spawn. This is pure embedded JS with no server
// path of its own, so guard the wiring against a silent drop in a future
// refactor. Mirrors TestDashboardHTML_SpawnNameNormalizeWired.
func TestDashboardHTML_SpawnNameOrDescrRequired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still contain %q (%s)", needle, why)
		}
	}

	// The gate: both-blank → inline error + return, never a spawn.
	must("if (!name && !descr) {", "gate fires only when name AND description are blank")
	must("give the agent a name or an initial description", "the inline error explains the requirement")

	// The earlier soft-confirm ("spawn anyway") path is gone — the requirement
	// is now hard, so its confirm copy (which was unique to this gate) must not
	// linger anywhere in the bundle.
	mustNot("Spawn without a name?", "the soft-confirm path was replaced by a hard requirement")
	mustNot("okLabel: 'Spawn anyway'", "no 'spawn anyway' escape hatch anymore")
}
