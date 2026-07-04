package agentd

import (
	"strings"
	"testing"
)

// A live agent / group can run a full model id that is not one of the curated
// Model-select preset aliases — e.g. "claude-opus-4-8[1m]", which ValidateModel
// accepts (fullModelIDRe) but the alias list never contains. Capturing such an
// agent into a spawn profile, or spawning from a profile that carries one, must
// keep that exact id SELECTABLE rather than silently dropping it on the
// <select>'s prior pick (a <select> ignores .value for an absent option).
// setModelSelectValue (helpers.js) injects the out-of-catalog id as an option
// before selecting it, and both the profile editor and the spawn dialog seed
// their Model control through it. There is no server path a flow test can drive
// for the <select> wiring, so — following the established dashboard_*_test.go
// structural guards — this pins its shape in the embedded JS.
func TestDashboardHTML_ModelCaptureOutOfCatalog(t *testing.T) {
	// The helper exists, is exported from helpers.js, keys its injected option
	// by a data attribute (so a re-open strips the stale one), and flags it so
	// an out-of-catalog id reads as such rather than a curated preset.
	for _, needle := range []string{
		"function setModelSelectValue(",
		"setModelSelectValue,",   // exported from helpers.js
		"o.dataset.dynamicModel", // stale-option cleanup key
		"(exact id)",             // out-of-catalog option label
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — out-of-catalog model wiring broken", needle)
		}
	}

	// Profile editor seeds the Model control through the helper, so a captured
	// (from-agent) or extracted (extractAgentToProfile) full model id stays
	// selected instead of dropping.
	if !strings.Contains(dashboardAssets, "setModelSelectValue(profileActiveModelEl(), seed.model)") {
		t.Error("openProfileEditor: must seed the Model control via setModelSelectValue so an out-of-catalog model stays selectable")
	}

	// Spawn dialog applies a profile's model through the helper too — a profile
	// carrying a non-default model must be selectable when spawning from it.
	if !strings.Contains(dashboardAssets, "setModelSelectValue(activeSpawnModelEl(), p.model)") {
		t.Error("applyProfileToSpawnForm: must apply the profile model via setModelSelectValue so a non-default model is selectable when spawning")
	}
}
