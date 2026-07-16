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
// The Preact spawn owner keeps the exact model string in its plain draft and
// derives whether the curated select displays it through the Custom sentinel.
func TestDashboardHTML_ModelCaptureOutOfCatalog(t *testing.T) {
	// The migrated profile editor uses a controlled free-text input backed by a
	// datalist, so an out-of-catalog id is represented directly in state.
	if !strings.Contains(dashboardAssets, "model: seed?.model || ''") ||
		!strings.Contains(dashboardAssets, "model: draft.model.trim()") {
		t.Error("Preact profile editor must seed and submit the exact model string")
	}

	// Spawn profiles preserve the exact string in controlled state and the
	// selector derives the Custom sentinel for values outside the catalog.
	for _, needle := range []string{
		"if (profile.model) {",
		"next.model = text(profile.model)",
		"next.customModel = view.hasModelList && !view.models.includes(next.model)",
		"return view.models.includes(draft.model) ? draft.model : MODEL_CUSTOM_VALUE",
	} {
		if strings.Contains(dashboardAssets, needle) {
			continue
		}
		t.Error("Preact spawn profile application must preserve an out-of-catalog model through the Custom sentinel")
		break
	}
}

// A curated Model <select> can't be typed into, so a human who wants a
// model the presets don't list (a brand-new alias, or a full id) had no way to
// enter one by hand. Each editor's <select> now ends with a "Custom model id…"
// sentinel <option> that reveals a free-text <input>; picking it routes submit
// and seeding through that input. This pins the wiring across the three editors
// (spawn dialog, profile editor, role editor) through controlled Preact state.
func TestDashboardHTML_CustomModelFreeText(t *testing.T) {
	if !strings.Contains(dashboardAssets, "export const MODEL_CUSTOM_VALUE = '__custom__';") {
		t.Error("Preact spawn model state must retain the Custom model sentinel")
	}

	// The Preact spawn editor retains the select+sentinel/custom-input contract.
	for _, base := range []string{"agent-spawn-model"} {
		for _, needle := range []string{
			`id="` + base + `-custom"`,     // the free-text input
			`id="` + base + `-custom-row"`, // its toggled row
			// The select routes to the custom input on the sentinel (reads:
			// submit + effort read the typed value). Compared against the Preact
			// MODEL_CUSTOM_VALUE constant, not a hardcoded literal.
			"modelSelectValue(draft, context)",
			"hidden=${selectedModel !== MODEL_CUSTOM_VALUE}",
			"document.querySelector('#agent-spawn-model-custom')?.focus()",
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
		"view.models.map((model)",
		"const models = hEntry?.models || []",
	} {
		if !strings.Contains(dashboardAssets, call) {
			t.Errorf("dashboard JS missing %q — per-harness model catalog is not wired across every editor", call)
		}
	}
	if !strings.Contains(dashboardAssets, "const keepModel = profile.harness === next.harness ? next.model : ''") ||
		!strings.Contains(dashboardAssets, "if (keepModel) next.model = keepModel") {
		t.Error("spawn harness re-apply must preserve a manual model when a sparse same-harness profile is applied")
	}

	if !strings.Contains(dashboardAssets, `<option value=${MODEL_CUSTOM_VALUE}>Custom model id…</option>`) {
		t.Error("Preact spawn selector must emit the Custom model sentinel")
	}

	// Each revealed free-text input carries an accessible name — its row's label
	// span is empty (grid alignment), so without this the input has no a11y name.
	if got := strings.Count(dashboardAssets, `aria-label="Custom model id"`); got != 2 {
		t.Errorf("expected accessible custom-model inputs for spawn and shared management editors, got %d", got)
	}
}
