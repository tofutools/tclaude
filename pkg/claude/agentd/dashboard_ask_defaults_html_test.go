package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAssets_AskDefaultsWired guards the Config tab's "Ask
// defaults" fields (JOH-253). They are ordinary widgets of the big config
// form — populated by populateConfigForm, read by assembleConfig, and
// saved through the existing /api/config dry-run/diff/confirm flow like
// every other field — NOT a separate endpoint with its own button. This
// pins that integration so a change to one file can't silently break it.
func TestDashboardAssets_AskDefaultsWired(t *testing.T) {
	for _, needle := range []string{
		// HTML: the section heading + the select anchors (incl. the
		// harness-independent Profile selector, JOH-252).
		// Ask and Scribe defaults share one section; the labels carry the
		// "Ask" qualifier that the old dedicated heading used to.
		"<h3>Ask &amp; scribe defaults</h3>",
		`<span class="cfg-label">Ask — profile</span>`,
		`id="ask-profile"`,
		`id="ask-model"`,
		`id="ask-effort"`,
		// JS: options come from the harness catalog / saved spawn profiles;
		// values populate and assemble into the cfg.ask block the big form
		// submits.
		"function populateAskSelects(",
		"function populateAskProfileSelect(",
		"setAskSelectValue($('#ask-model')",
		"setAskSelectValue($('#ask-effort')",
		"populateAskProfileSelect(ask.profile)",
		"controlValue($('#ask-model')).trim()",
		"controlValue($('#ask-effort')).trim()",
		"ask.profile = askProfile;",
		"cfg.ask = ask;",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Ask defaults wiring broken", needle)
		}
	}

	// The dedicated-endpoint design was replaced by the big-form
	// integration: none of its wiring may linger, or two save paths would
	// fight over the same config block.
	for _, gone := range []string{
		"/api/ask-profile",
		"ask-profile.js",
		"ask-profile-apply",
		"config-disk-changed",
	} {
		if strings.Contains(dashboardAssets, gone) {
			t.Errorf("dashboard assets still reference removed ask-profile wiring %q", gone)
		}
	}
}
