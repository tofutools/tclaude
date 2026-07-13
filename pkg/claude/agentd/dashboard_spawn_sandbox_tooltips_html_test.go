package agentd

import (
	"strings"
	"testing"
)

// The Sandbox and Sandbox profile explanations can be long, especially the
// composed-policy preview. Keep them available on hover and to assistive
// technology without rendering persistent help rows in the spawn dialog.
func TestDashboardHTML_SpawnSandboxHelpUsesTooltips(t *testing.T) {
	for needle, why := range map[string]string{
		`id="agent-spawn-sandbox-hint" class="spawn-field-tooltip-copy"`:            "sandbox mode help is visually hidden",
		`aria-describedby="agent-spawn-sandbox-profile-preview"`:                    "sandbox profile tooltip has an accessible description",
		`id="agent-spawn-sandbox-profile-preview" class="spawn-field-tooltip-copy"`: "sandbox profile preview is visually hidden",
		`selectEl.title = text`: "selected sandbox mode help updates the dropdown tooltip",
		`const setPreview = (text) => { select.title = text; preview.textContent = text; }`: "composed policy updates the profile dropdown tooltip and accessible copy",
		`.spawn-field-tooltip-copy {`: "hidden accessible tooltip-copy style exists",
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
