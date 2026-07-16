package agentd

import (
	"strings"
	"testing"
)

// The spawn modal requires a name OR an initial description so the new agent is
// identifiable. With both blank there are two outcomes, both gated behind the
// same `if (!usableName && !descr)` check in the spawn model:
//
//   - An initial message was typed → derive a name from its first few words
//     (deriveSpawnNameFromMessage) and confirm before spawning. The human
//     confirms they don't want to pick a name by hand; the agent is auto-named.
//   - No usable message either → the hard inline error fires and re-focuses the
//     Name field; the spawn does NOT go through.
//
// This is pure embedded JS with no server path of its own, so guard the wiring
// against a silent drop in a future refactor. Mirrors
// TestDashboardHTML_SpawnNameNormalizeWired.
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

	// The gate still fires only when BOTH the normalized usable name and
	// description are blank. Symbol-only input must not become a nameless spawn.
	must("!usableName && !text(draft.descr).trim()", "gate fires only when usable name AND description are blank")

	// With an initial message, derive a name from its first words and confirm
	// before spawning — rather than rejecting.
	must("function deriveSpawnNameFromMessage(", "the helper that builds a name from the initial message exists")
	must("deriveSpawnNameFromMessage(next.initialMessage)", "the submit path derives a name from the initial message")
	must("Auto-name this agent?", "the confirm explains the auto-name before spawning")

	// With no derivable name (blank/symbol-only message), the hard inline error
	// still fires and the spawn does not go through.
	must("give the agent a name or an initial description", "the inline error explains the requirement when nothing is derivable")

	// The earlier soft-confirm ("spawn anyway") copy from #562 stays gone — the
	// new confirm is the auto-name one, not a blanket "spawn anyway" escape hatch.
	mustNot("Spawn without a name?", "the old soft-confirm path is not resurrected")
	mustNot("okLabel: 'Spawn anyway'", "no 'spawn anyway' escape hatch")
}
