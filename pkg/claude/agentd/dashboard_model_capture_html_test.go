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

	// The migrated profile editor uses a controlled free-text input backed by a
	// datalist, so an out-of-catalog id is represented directly in state.
	if !strings.Contains(dashboardAssets, "model: seed?.model || ''") ||
		!strings.Contains(dashboardAssets, "model: draft.model.trim()") {
		t.Error("Preact profile editor must seed and submit the exact model string")
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

// A curated Model <select> can't be typed into, so a human who wants a
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

	// The still-imperative spawn editor retains the select+sentinel helper.
	for _, base := range []string{"agent-spawn-model"} {
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
	// Preact profile/role editors retain the curated dropdown for harnesses with
	// a model catalog, plus a controlled custom-ID fallback. Harnesses without a
	// catalog use the free-text branch directly.
	for _, needle := range []string{
		`const modelID = profile ? 'profile-editor-model' : 'role-editor-model'`,
		`const modelControl = hasModelList ? html`,
		`['__custom__', 'Custom model id…']`,
		`setCustomModel(true)`,
		"id=${`${modelID}-custom`}",
		`onInput=${(event) => change(setDraft, 'model', event.currentTarget.value)}`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — Preact model input wiring broken", needle)
		}
	}

	// Each editor rebuilds that shared selector from the selected harness's
	// catalog, so Codex and Claude receive the same preset + custom-ID behavior.
	for _, call := range []string{
		"populateModelSelect($('#agent-spawn-model'), h.models)",
		"const models = hEntry?.models || []",
	} {
		if !strings.Contains(dashboardAssets, call) {
			t.Errorf("dashboard JS missing %q — per-harness model catalog is not wired across every editor", call)
		}
	}
	if !strings.Contains(dashboardAssets, "appliedSpawnHarness === harnessName") ||
		!strings.Contains(dashboardAssets, "setModelSelectValue($('#agent-spawn-model'), keepModel)") {
		t.Error("spawn harness re-apply must preserve a manual model when a sparse same-harness profile is applied")
	}

	// The spawn selector remains static HTML; the shared Preact selector emits
	// its sentinel dynamically from the options array above.
	if got := strings.Count(dashboardAssets, `<option value="__custom__">Custom model id…</option>`); got != 1 {
		t.Errorf("expected one spawn-dialog custom-model sentinel, got %d", got)
	}

	// Each revealed free-text input carries an accessible name — its row's label
	// span is empty (grid alignment), so without this the input has no a11y name.
	if got := strings.Count(dashboardAssets, `aria-label="Custom model id"`); got != 2 {
		t.Errorf("expected accessible custom-model inputs for spawn and shared management editors, got %d", got)
	}
}
