package agentd

import (
	"strings"
	"testing"
)

// TestDashboardCSS_StylingSweep pins the JOH-364 dashboard styling sweep: a set
// of dialog / row / form controls that rendered the browser's white UA chrome
// (or a mis-sized box) in plain, non-wizard mode because dashboard.css had no
// base `button {}` reset and no base `button.tool` rule — the wizard sweep had
// only given many of them body.wizard-scoped colors. The fix is additive CSS: a
// global fallback `button.tool` skin plus targeted rules for the controls that
// live outside any themed container.
//
// String tests can't see rendered color, so these pins only guard PRESENCE — a
// refactor that drops one of the new base rules regresses the corresponding
// control back to white and fails here. The by-eye both-skins evidence lives in
// the PR, not in this file.
func TestDashboardCSS_StylingSweep(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source unexpectedly contains %q (%s)", needle, why)
		}
	}

	// Group 1 — global FALLBACK skin for `.tool` buttons (A1 colors + the
	// template / profile / role card, starter-list and template-editor tools).
	// Anchored at line start so it does not match the scoped `.filter-bar
	// button.tool {` / `.mail-list-filter button.tool {` rules that share the
	// `button.tool {` suffix.
	must("\nbutton.tool {", "the global fallback button.tool skin exists")
	// Hover sets ONLY border-color so it can't leak a bg/text change onto the
	// scoped (do-not-touch) toolbar buttons — the `; }` right after the colour
	// distinguishes it from `.filter-bar button.tool:hover { …; color: … }`.
	must("\nbutton.tool:hover { border-color: #58a6ff; }", "the fallback hover is border-only (leak-free)")
	// The deploy/summon action is a `.primary`, not a `.tool`.
	must(".template-card .tc-actions button.primary {", "the template deploy .primary button is themed")

	// Group 2 — A1 sizing (both skins): the "⧉ manage templates…" tool button
	// beside the template <select> gets the dir-browse flex idiom so it matches
	// the select's height instead of collapsing to its intrinsic height.
	must(".cron-create-row > button.tool {", "the manage-templates button gets the dir-browse sizing idiom")

	// Group 3 — A7 template-editor number inputs: the per-agent .ta-wave field
	// (folded into the .template-agent-row rule) and the standalone wave-max-wait
	// field (its own .template-editor-field rule).
	must(".template-agent-row input[type=number],", "the .ta-wave number input is folded into the row-input rule")
	must(".template-editor-field input[type=number] {", "the wave-max-wait number field is themed")

	// Group 4 — A8 mail-reader action bar: reply / focus / mark / delete are
	// plain <button>s with no .tool class, so they needed a base rule.
	must(".mail-reader-actions button {", "the mail-reader action bar has a base button skin")

	// Group 5 — B1/B2/B3 buttons that sit outside a .row-actions wrapper.
	must(".plugin-step-edit-head button,", "the plugin step-editor remove button is themed")
	must(".plugin-head > button.primary,", "the catalog +install button is themed (direct child only)")
	must("#sudo-list td > button {", "the sudo-grants revoke button is themed")

	// Group 6 — B4/B5/B6 remote-access controls (both skins for buttons; plain +
	// the wizard twin for the fields).
	must(".ra-inline-form button {", "the remote-access action buttons are themed")
	must(".cfg-field input[type=password],", "password fields join the cfg-field input rule")
	must(".cfg-field select, .ra-form-grid input {", "the .ra-form-grid setup inputs join the cfg-field input rule")
	must("body.wizard #tab-config .cfg-field input[type=password],", "the wizard twin also covers password fields")
	must("body.wizard #tab-config .ra-form-grid input,", "the wizard twin also covers the .ra-form-grid inputs")

	// C1 — the spawn-dialog attachment ✕ stays a ghost button in the wizard skin:
	// the blanket repaint now excludes .att-remove.
	must("button:not(#agent-spawn-submit):not(.att-remove) {", "the wizard spawn blanket excludes the ghost att-remove button")

	// Anti-regression: the plain-mode gaps must NOT be closed by widening a
	// body.wizard rule (the wizard selectors stay modal-id-scoped). A bare
	// `button.tool { ... }` fallback with a background hover would leak onto the
	// scoped toolbars — guard that the fallback hover never gained a background.
	mustNot("button.tool:hover { background:", "the fallback tool hover must not set a background (would leak onto scoped toolbars)")
}
