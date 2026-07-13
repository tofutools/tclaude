package agentd

import "testing"

// TestDashboardHTML_ScribeButtons pins the "Edit with agent" entry points
// (JOH-361): a library-scope button in the Templates overlay header and a
// template-scope button in the editor footer, each summoning a pre-briefed,
// pre-granted scribe. String-pins because the buttons are static HTML + wired
// JS with no server render to assert through — a refactor that drops an id, the
// wizard label pair, or the summon wiring would otherwise silently unhook the
// feature.
func TestDashboardHTML_ScribeButtons(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Both entry-point buttons exist by id.
	must(`id="scribe-templates-open"`, "the Templates-header library-scope Edit-with-agent button")
	must(`id="scribe-editor-open"`, "the editor footer template-scope Edit-with-agent button")

	// The wizard label pair (plain boring + lore-coherent scribe wizardry), the
	// same span-swap idiom the neighbouring header buttons use. Present on both
	// buttons; a single Contains suffices.
	must(`<${Words} plain="🤖 Edit with agent" wizard="📜 Dictate to a scribe"/>`,
		"the wizard-mode label swap for the Edit-with-agent buttons")

	// The editor button must NOT be #template-editor-submit, so the wizard
	// blanket `body.wizard #template-editor-modal button:not(#template-editor-submit)`
	// re-skins it for free — guard that it sits in the editor's modal-buttons row
	// as a plain (non-submit) button.
	must(`<button id="scribe-editor-open" type="button"`, "editor scribe button is a plain (non-submit) button")

	// JS wiring: the summon POSTs to the generic daemon endpoint with the base
	// name (the daemon adds a unique suffix) and the minimal grant bundle.
	must(`fetch('/api/scribe'`, "the summon POSTs to the generic /api/scribe endpoint")
	must(`const SCRIBE_NAME = 'circle-scribe';`, "the stable scribe-kind base name")
	must(`const SCRIBE_SLUGS = ['templates.manage'];`, "the minimal template-scribe grant bundle")

	// The editor entry point consults the dirty flag before handing off — an
	// open dirty editor would stomp the scribe's full-replace edits on save.
	must(`dirty && !(await confirmDiscard())`, "the editor summon guards unsaved edits before handing off")
}
