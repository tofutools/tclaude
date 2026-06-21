package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAssets_AskProfileWired guards the Config tab's "Ask
// defaults" section (JOH-253): the HTML form anchors, the ES-module that
// reads/writes them through /api/ask-profile, its wiring into the
// dashboard entrypoint, and the config-tab baseline-resync handshake.
// Asserting on the embedded concatenation catches a rename in any one
// file that would silently break the feature in the browser.
func TestDashboardAssets_AskProfileWired(t *testing.T) {
	for _, needle := range []string{
		// HTML: the section heading + the three form anchors the module binds.
		"<h3>Ask defaults</h3>",
		`id="ask-model"`,
		`id="ask-effort"`,
		`id="ask-profile-apply"`,
		`id="ask-profile-status"`,
		// JS: the module talks to the dedicated endpoint and is exported.
		"fetch('/api/ask-profile'",
		"function bindAskProfileSection(",
		// Wiring: imported + invoked from the dashboard entrypoint.
		"import { bindAskProfileSection } from './ask-profile.js';",
		"bindAskProfileSection();",
		// Handshake: the apply fires the event, config.js resyncs its baseline.
		"new CustomEvent('config-disk-changed')",
		"addEventListener('config-disk-changed', syncConfigBaseline)",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Ask defaults wiring broken", needle)
		}
	}
}
