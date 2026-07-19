package agentd

import (
	"strings"
	"testing"
)

// Long sandbox help remains available through native select titles and an
// explicit accessible disclosure owned by the Preact component.
func TestDashboardHTML_SpawnSandboxHelpUsesNativeTooltips(t *testing.T) {
	for needle, why := range map[string]string{
		`class="spawn-field-help-trigger"`:                                  "explicit keyboard/touch disclosure",
		`aria-controls=${descriptionID} aria-expanded=${open ? 'true' : 'false'}`: "disclosure publishes expanded state",
		`<span id=${descriptionID} class="spawn-field-description" role="tooltip" tabindex="0"`: "help is focusable and scrollable",
		`<label class="cron-create-label" for=${id}>`:                         "selector retains an explicit label",
		`title=${help} aria-describedby=${descriptionID}`:                      "select receives native and accessible help",
		`descriptionID="agent-spawn-sandbox-profile-preview"`:                "sandbox-profile preview keeps its stable id",
		`onClick=${() => setOpen(open ? '' : id)}`:                            "click/tap toggles the disclosure",
		`onFocus=${() => setOpen(id)}`:                                        "keyboard focus opens the disclosure",
		`.spawn-field-description {`:                                          "accessible descriptions are visually hidden",
		`.spawn-field-help-trigger[aria-expanded="true"] + .spawn-field-description,`: "expanded disclosure becomes visible",
		`max-height: min(180px, 30vh); overflow: auto;`:                        "long help remains scrollable",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}
	for _, obsolete := range []string{
		`id="agent-spawn-sandbox-hint" class="spawn-field-hint"`,
		`id="agent-spawn-sandbox-profile-preview" class="spawn-field-hint"`,
		`class="spawn-field-tooltip-copy"`,
	} {
		if strings.Contains(dashboardAssets, obsolete) {
			t.Errorf("spawn dialog still renders persistent sandbox help: %q", obsolete)
		}
	}
}
