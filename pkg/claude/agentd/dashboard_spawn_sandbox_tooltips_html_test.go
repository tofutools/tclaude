package agentd

import (
	"strings"
	"testing"
)

// Long sandbox help remains available through native select titles and an
// explicit accessible disclosure owned by the Preact component.
func TestDashboardHTML_SpawnSandboxHelpUsesNativeTooltips(t *testing.T) {
	for needle, why := range map[string]string{
		`class="spawn-field-help-trigger"`:                                                      "explicit keyboard/touch disclosure",
		`aria-controls=${descriptionID} aria-expanded=${open ? 'true' : 'false'}`:               "disclosure publishes expanded state",
		`<span id=${descriptionID} class="spawn-field-description" role="tooltip" tabindex="0"`: "help is focusable and scrollable",
		`<label class="cron-create-label" for=${id}>`:                                           "selector retains an explicit label",
		`title=${help} aria-describedby=${descriptionID}`:                                       "select receives native and accessible help",
		`descriptionID="agent-spawn-sandbox-profile-preview"`:                                   "sandbox-profile preview keeps its stable id",
		`onClick=${() => setOpen(open ? '' : id)}`:                                              "click/tap toggles the disclosure",
		`onFocus=${() => setOpen(id)}`:                                                          "keyboard focus opens the disclosure",
		`.spawn-field-description {`:                                                            "accessible descriptions are visually hidden",
		`.spawn-field-help-trigger[aria-expanded="true"] + .spawn-field-description,`:           "expanded disclosure becomes visible",
		`max-height: min(180px, 30vh); overflow: auto;`:                                         "long help remains scrollable",
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

// Every dropdown whose help is static per-mode documentation routes through
// HelpField, in both the spawn dialog and the profile/role editors. The only
// hint left permanently on screen is the name field's, which is live
// validation feedback rather than documentation.
func TestDashboardHTML_ModeHelpUsesHelpField(t *testing.T) {
	for needle, why := range map[string]string{
		`import { HelpField } from './help-field.js';`:                    "HelpField is shared, not private to the spawn island",
		`id="agent-spawn-approval" label=${draft.harness`:                 "spawn approval policy uses the [?] disclosure",
		`id="agent-spawn-ask-timeout" label="Question timeout"`:           "spawn question timeout uses the [?] disclosure",
		`id=${approvalID} label=${approvalLabel}`:                         "profile/role approval policy uses the [?] disclosure",
		`id=${sandboxID} label="Sandbox"`:                                 "profile/role sandbox gained help it never had",
		`id="profile-editor-approval-reviewer" label="Approval reviewer"`: "profile approval reviewer uses the [?] disclosure",
		`id="profile-editor-ask-timeout" label="Question timeout"`:        "profile question timeout uses the [?] disclosure",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}
	for _, obsolete := range []string{
		`id="agent-spawn-approval-hint"`,
		`id="agent-spawn-ask-timeout-hint"`,
		`id="profile-editor-approval-reviewer-hint"`,
		`id=${` + "`" + `${approvalID}-hint` + "`" + `}`,
	} {
		if strings.Contains(dashboardAssets, obsolete) {
			t.Errorf("mode help is still rendered as a persistent paragraph: %q", obsolete)
		}
	}
}

// A "⚠" in mode help is a safety caveat — that the mode can deadlock a
// detached agent or drop guardrails. Collapsing it behind the [?] would hide
// it, so HelpField keeps just that sentence visible below the select.
func TestDashboardHTML_WarningHelpStaysVisible(t *testing.T) {
	for needle, why := range map[string]string{
		`export function helpCaveat(help)`:                 "the caveat extractor is exported and testable",
		`const start = text.indexOf('⚠');`:                 "caveats are keyed off the ⚠ marker",
		`const caveat = helpCaveat(help);`:                 "HelpField splits the caveat out of the collapsed copy",
		`class="spawn-field-hint warn spawn-field-caveat"`: "the caveat renders with the warn styling",
		`.spawn-field-caveat { grid-column: 1 / -1; }`:     "the caveat spans under both the select and the [?]",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard missing %q (%s)", needle, why)
		}
	}
}
