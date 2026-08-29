package agentd

import "testing"

// Environment guidance remains available on demand without reserving permanent
// paragraphs in the spawn, profile, or group-settings dialogs.
func TestDashboardHTML_EnvironmentHelpUsesCompactDisclosures(t *testing.T) {
	for needle, why := range map[string]string{
		`id="agent-spawn-environment-help"`:             "spawn overrides have a help disclosure",
		`id="agent-spawn-effective-environment-help"`:   "effective spawn values have a help disclosure",
		`id="agent-spawn-effective-environment-toggle"`: "effective spawn values are collapsed behind an explicit toggle",
		`id="profile-editor-environment-help"`:          "profile variables have a help disclosure",
		`id="group-settings-environment-help"`:          "group variables have a help disclosure",
		`class="sbx-add-row-help"`:                      "environment editors put help beside their add controls",
		`.sbx-effective-environment-help`:               "the effective environment preview has a compact help anchor",
		`ENVIRONMENT_OVERRIDE_HELP`:                     "spawn precedence copy remains available",
		`EFFECTIVE_ENVIRONMENT_HELP`:                    "effective-value resolution copy remains available",
		`PROFILE_ENVIRONMENT_HELP`:                      "profile precedence copy remains available",
		`GROUP_ENVIRONMENT_HELP`:                        "group precedence copy remains available",
	} {
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	for obsolete, why := range map[string]string{
		`<div class="muted"><${Words} plain="Explicit values override`:                "spawn precedence copy is still always visible",
		`<div class="muted"><${Words} plain="Common configured values shown here`:     "effective-value resolution copy is still always visible",
		`<div class="spawn-field-hint"><${Words} plain="Applied to every fresh spawn`: "profile precedence copy is still always visible",
		`<div class="spawn-field-hint"><${Words} plain="Inherited by fresh spawns`:     "group precedence copy is still always visible",
	} {
		if dashboardSourceContains(dashboardAssets, obsolete) {
			t.Errorf("%s: %q", why, obsolete)
		}
	}
}
