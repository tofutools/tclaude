package agentd

import (
	"strings"
	"testing"
)

// The Sandbox and Sandbox profile explanations can be long, especially the
// composed-policy preview. Keep them on the same native tooltip path as other
// selects and available to assistive technology without persistent help rows.
func TestDashboardHTML_SpawnSandboxHelpUsesNativeTooltips(t *testing.T) {
	for needle, why := range map[string]string{
		`id="agent-spawn-sandbox-hint" class="spawn-field-description" aria-live="polite"`: "sandbox mode help remains an accessible live description",
		`for="agent-spawn-sandbox"`: "sandbox selector retains an explicit label",
		`id="agent-spawn-sandbox" title="" aria-describedby="agent-spawn-sandbox-hint"`:               "sandbox selector receives its selected mode's native title",
		`aria-describedby="agent-spawn-sandbox-profile-preview"`:                                      "sandbox profile selector retains its accessible description",
		`id="agent-spawn-sandbox-profile-preview" class="spawn-field-description" aria-live="polite"`: "sandbox profile preview remains an accessible live description",
		`if (option) option.title = text`:                                                             "selected options carry the native tooltip copy",
		`syncSelectTitle(selectEl)`:                                                                   "sandbox mode uses the shared select title helper",
		`syncSelectTitle(select);`:                                                                    "composed policy uses the shared select title helper",
		`.spawn-field-description {`:                                                                  "accessible descriptions are visually hidden",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}

	for _, visibleHint := range []string{
		`id="agent-spawn-sandbox-hint" class="spawn-field-hint"`,
		`id="agent-spawn-sandbox-profile-preview" class="spawn-field-hint"`,
		`class="spawn-field-tooltip-copy"`,
		`.cron-create-target > select:hover + .spawn-field-tooltip-copy`,
	} {
		if strings.Contains(dashboardAssets, visibleHint) {
			t.Errorf("spawn dialog still renders persistent sandbox help: %q", visibleHint)
		}
	}
}
