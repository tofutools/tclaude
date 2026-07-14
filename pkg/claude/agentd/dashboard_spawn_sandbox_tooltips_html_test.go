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
		`class="spawn-field-help-trigger" aria-label="Show Sandbox help" aria-controls="agent-spawn-sandbox-hint" aria-expanded="false"`: "sandbox mode has an explicit keyboard/touch disclosure",
		`id="agent-spawn-sandbox-hint" class="spawn-field-description" role="tooltip" tabindex="0"`:                                      "sandbox mode help is focusable and scrollable",
		`for="agent-spawn-sandbox"`: "sandbox selector retains an explicit label",
		`id="agent-spawn-sandbox" title="" aria-describedby="agent-spawn-sandbox-hint"`:                                    "sandbox selector receives its selected mode's native title",
		`aria-describedby="agent-spawn-sandbox-profile-preview"`:                                                           "sandbox profile selector retains its accessible description",
		`aria-label="Show Sandbox profile help" aria-controls="agent-spawn-sandbox-profile-preview" aria-expanded="false"`: "sandbox profile has an explicit keyboard/touch disclosure",
		`id="agent-spawn-sandbox-profile-preview" class="spawn-field-description" role="tooltip" tabindex="0"`:             "sandbox profile preview is focusable and scrollable",
		`if (option) option.title = text`: "selected options carry the native tooltip copy",
		`syncSelectTitle(selectEl)`:       "sandbox mode uses the shared select title helper",
		`syncSelectTitle(select);`:        "composed policy uses the shared select title helper",
		`.spawn-field-description {`:      "accessible descriptions are visually hidden",
		`.spawn-field-help-trigger[aria-expanded="true"] + .spawn-field-description,`: "explicit click/tap state opens the visible overlay",
		`trigger.setAttribute('aria-expanded', String(open))`:                         "click/tap toggles help independently of browser focus behavior",
		`if (trigger && trigger.matches(':focus-visible')) {`:                         "keyboard focus updates the same explicit open state",
		`trigger.setAttribute('aria-expanded', 'true')`:                               "keyboard-visible help matches its announced expanded state",
		`max-height: min(180px, 30vh); overflow: auto;`:                               "long focused help remains scrollable",
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
