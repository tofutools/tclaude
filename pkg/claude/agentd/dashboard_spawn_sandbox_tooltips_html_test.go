package agentd

import (
	"strings"
	"testing"
)

// The Sandbox and Sandbox profile explanations can be long, especially the
// composed-policy preview. Keep them available on hover, focus/tap, and to
// assistive technology without rendering persistent help rows in the dialog.
func TestDashboardHTML_SpawnSandboxHelpUsesTooltips(t *testing.T) {
	for needle, why := range map[string]string{
		`id="agent-spawn-sandbox-hint" class="spawn-field-tooltip-copy" role="tooltip" tabindex="0" title=""`: "sandbox mode help is a focusable tooltip without an inherited native title",
		`for="agent-spawn-sandbox"`: "sandbox selector retains an explicit label",
		`id="agent-spawn-sandbox" title="" aria-describedby="agent-spawn-sandbox-hint"`:                                  "custom sandbox tooltip suppresses inherited native title",
		`aria-describedby="agent-spawn-sandbox-profile-preview"`:                                                         "sandbox profile tooltip has an accessible description",
		`id="agent-spawn-sandbox-profile-preview" class="spawn-field-tooltip-copy" role="tooltip" tabindex="0" title=""`: "sandbox profile preview is a focusable tooltip without an inherited native title",
		`hintEl.textContent = text`:                                        "selected sandbox mode updates its tooltip",
		`const setPreview = (text) => { preview.textContent = text; }`:     "composed policy updates the profile tooltip",
		`.cron-create-target > select:hover + .spawn-field-tooltip-copy {`: "mouse hover reveals dropdown help",
		`.cron-create-target > select:focus + .spawn-field-tooltip-copy,`:  "keyboard focus and touch reveal dropdown help",
		`max-height: min(180px, 30vh); overflow: auto;`:                    "long policy previews remain scrollable",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}

	for _, visibleHint := range []string{
		`id="agent-spawn-sandbox-hint" class="spawn-field-hint"`,
		`id="agent-spawn-sandbox-profile-preview" class="spawn-field-hint"`,
	} {
		if strings.Contains(dashboardAssets, visibleHint) {
			t.Errorf("spawn dialog still renders persistent sandbox help: %q", visibleHint)
		}
	}
}
