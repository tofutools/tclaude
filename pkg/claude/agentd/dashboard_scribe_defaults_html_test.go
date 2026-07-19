package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAssets_ScribeDefaultsWired guards the Config tab's "Scribe
// defaults" field (JOH-371). Like the Ask-defaults block it mirrors, the
// scribe-profile selector is an ordinary widget of the big config form —
// populated by populateConfigForm, read by assembleConfig, and saved through
// the existing /api/config dry-run/diff/confirm flow like every other field —
// NOT a separate endpoint with its own button. This pins that integration so a
// change to one file can't silently break it.
func TestDashboardAssets_ScribeDefaultsWired(t *testing.T) {
	for _, needle := range []string{
		// HTML: the section heading + the profile select anchor. Ask and
		// Scribe defaults share one section; the label carries the "Scribe"
		// qualifier that the old dedicated heading used to.
		"<h3>Ask &amp; scribe defaults</h3>",
		`<span class="cfg-label">Scribe — profile</span>`,
		`id="scribe-profile"`,
		// JS: options come from the saved spawn profiles; the value populates
		// and assembles into the cfg.scribe block the big form submits.
		"function populateScribeProfileSelect(",
		"populateScribeProfileSelect((cfg.scribe || {}).profile)",
		"$('#scribe-profile')",
		"scribe.profile = scribeProfile;",
		"cfg.scribe = scribe;",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — Scribe defaults wiring broken", needle)
		}
	}

	// Scribe defaults reuse the big-form save path (no dedicated endpoint /
	// per-field button), mirroring the Ask-defaults design so two save paths
	// never fight over the same config block.
	for _, gone := range []string{
		"/api/scribe-profile",
		"scribe-profile.js",
		"scribe-profile-apply",
	} {
		if strings.Contains(dashboardAssets, gone) {
			t.Errorf("dashboard assets still reference a non-existent scribe-profile endpoint %q", gone)
		}
	}
}
