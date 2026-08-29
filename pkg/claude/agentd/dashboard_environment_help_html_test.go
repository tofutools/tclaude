package agentd

import "testing"

// Environment precedence remains available on demand without reserving a
// permanent paragraph in either of the two launch-profile authoring surfaces.
func TestDashboardHTML_EnvironmentHelpUsesCompactDisclosures(t *testing.T) {
	for needle, why := range map[string]string{
		`id="agent-spawn-environment-help"`:           "spawn overrides have a help disclosure",
		`id="profile-editor-environment-help"`:        "profile variables have a help disclosure",
		`class="sbx-add-row-help"`:                    "the help trigger sits beside its add control",
		`.sbx-add-row-help .spawn-field-help-trigger`: "the compact disclosure receives bounded sizing",
		`ENVIRONMENT_OVERRIDE_HELP`:                   "spawn precedence copy remains available",
		`PROFILE_ENVIRONMENT_HELP`:                    "profile precedence copy remains available",
	} {
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	for obsolete, why := range map[string]string{
		`<div class="muted"><${Words} plain="Explicit values override`:                "spawn precedence copy is still always visible",
		`<div class="spawn-field-hint"><${Words} plain="Applied to every fresh spawn`: "profile precedence copy is still always visible",
	} {
		if dashboardSourceContains(dashboardAssets, obsolete) {
			t.Errorf("%s: %q", why, obsolete)
		}
	}
}
