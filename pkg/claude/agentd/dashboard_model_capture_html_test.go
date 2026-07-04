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
	// carrying a non-default model must be selectable when spawning from it. It
	// routes to the curated <select> explicitly (not activeSpawnModelEl, which
	// may point at the "Custom…" free-text input) so a stale custom entry can't
	// swallow the profile's model.
	if !strings.Contains(dashboardAssets, "setModelSelectValue($('#agent-spawn-model'), p.model)") {
		t.Error("applyProfileToSpawnForm: must apply the profile model to the curated <select> via setModelSelectValue so a non-default model is selectable when spawning")
	}
}

// The curated Claude Model <select> can't be typed into, so a human who wants a
// model the presets don't list (a brand-new alias, or a full id) had no way to
// enter one by hand. Each editor's <select> now ends with a "Custom model id…"
// sentinel <option> that reveals a free-text <input>; picking it routes submit
// and seeding through that input. This pins the wiring across the three editors
// (spawn dialog, profile editor, role editor) — HTML control, the shared helper,
// the active-element resolver's sentinel branch, and the change listener.
func TestDashboardHTML_CustomModelFreeText(t *testing.T) {
	// Shared helper + sentinel exist and are exported from helpers.js.
	for _, needle := range []string{
		"const MODEL_CUSTOM_VALUE = '__custom__';",
		"function syncCustomModelRow(",
		"MODEL_CUSTOM_VALUE,", // exported
		"syncCustomModelRow,", // exported
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — custom-model free-text helper broken", needle)
		}
	}

	// Every curated Claude Model <select> ends with the sentinel option, and
	// each editor carries the revealed free-text input + its row. Missing any
	// leg silently breaks the feature in that one editor.
	for _, base := range []string{"agent-spawn-model", "profile-editor-model", "role-editor-model"} {
		for _, needle := range []string{
			`id="` + base + `-custom"`,     // the free-text input
			`id="` + base + `-custom-row"`, // its toggled row
			// The select routes to the custom input on the sentinel (reads:
			// submit + effort read the typed value). Compared against the shared
			// MODEL_CUSTOM_VALUE constant, not a hardcoded literal.
			"sel.value === MODEL_CUSTOM_VALUE ? $('#" + base + "-custom')",
			// The harness reshape reconciles the row with the select.
			"syncCustomModelRow('" + base + "')",
			// Picking "Custom…" reveals + focuses the row.
			"syncCustomModelRow('" + base + "', { focus: true })",
		} {
			if !strings.Contains(dashboardAssets, needle) {
				t.Errorf("dashboard JS/HTML missing %q — custom-model wiring broken for %s", needle, base)
			}
		}
	}

	// The sentinel <option> must appear once per editor (3 curated selects).
	if got := strings.Count(dashboardAssets, `<option value="__custom__">Custom model id…</option>`); got != 3 {
		t.Errorf("expected 3 \"Custom model id…\" sentinel options (spawn/profile/role), got %d", got)
	}

	// Each revealed free-text input carries an accessible name — its row's label
	// span is empty (grid alignment), so without this the input has no a11y name.
	if got := strings.Count(dashboardAssets, `aria-label="Custom model id"`); got != 3 {
		t.Errorf("expected 3 aria-labelled custom-model inputs (spawn/profile/role), got %d", got)
	}
}
