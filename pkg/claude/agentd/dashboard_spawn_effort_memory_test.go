package agentd

import (
	"strings"
	"testing"
)

// Per-model effort memory remains a server-backed dashboard preference. The
// plain spawn actions own persistence and the controlled component reapplies
// it on model changes; behavioural coverage lives in the Node spawn suite.
func TestDashboardHTML_SpawnEffortMemory(t *testing.T) {
	for _, needle := range []string{
		"const EFFORT_KEY = 'tclaude.dash.spawn.modelEffort';",
		"function readEffortMap(prefs)",
		"rememberedEffort(model)",
		"rememberLaunchPreferences(draft)",
		"if (draft.effort) map[draft.model || ''] = draft.effort",
		"effort: rememberedEffort(value)",
		"actions.rememberLaunchPreferences(next)",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — per-model effort memory wiring broken", needle)
		}
	}
}
