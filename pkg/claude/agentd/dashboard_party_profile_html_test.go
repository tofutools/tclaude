package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_PartyProfilePicker pins the JOH-356 "Form a party" party-
// profile picker in the embedded dashboard source: the create-group dialog can
// start from a template / summoning circle. The wiring lands across HTML (the
// dropdown, the manage affordance, the Task row + roster preview), CSS (the
// wizard skin for the new <select> + preview) and JS (prefill, preview,
// instantiate routing with context_override, and the redirected cog shortcut),
// in BOTH vocab modes. All client-side, so — like the other dashboard render
// guards — this string-searches the embedded source rather than running the JS.
func TestDashboardHTML_PartyProfilePicker(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// (a) HTML: the party-profile dropdown, its manage affordance, the template-
	// only Task row + roster preview row, and the id the Max-members row needs so
	// it can be hidden in template mode.
	must(`id="group-create-template"`, "the party-profile dropdown exists")
	must(`id="group-create-manage-templates"`, "the ⧉ manage-circles affordance exists")
	must(`<span class="tpl-word-regular">Party profile</span><span class="tpl-word-wizard">Summoning circle</span>`, "the picker label swaps per vocab mode")
	must(`<span class="tpl-word-regular">⧉ manage templates…</span><span class="tpl-word-wizard">⧉ manage circles…</span>`, "the manage button label swaps per vocab mode")
	must(`id="group-create-task-row"`, "the template-only Task row exists")
	must(`id="group-create-task"`, "the Task textarea exists")
	must(`id="group-create-template-preview-row"`, "the roster preview row exists")
	must(`id="group-create-template-preview"`, "the roster preview host exists")
	must(`id="group-create-source-row"`, "the mirror-source row exists")
	must(`id="group-create-parent-row"`, "the subgroup checkbox row exists")
	must(`id="group-create-max-members-row"`, "the Max-members row is id'd so it can hide in template mode")

	// (b) CSS: the new <select> and the roster readback get the wizard skin,
	// scoped to #group-create-modal (an unscoped rule would repaint the sibling
	// dialogs). The badge row has its own colour.
	must("body.wizard #group-create-modal .cron-create-row select {", "the party-profile select is wizard-skinned")
	must("body.wizard #group-create-modal .template-preview {", "the roster readback is wizard-skinned")
	must(".template-preview .tp-badges .tc-count {", "the readback badge chips are styled")

	// (c) JS: prefill + preview + instantiate routing carrying the group's own
	// edited context copy, and the "(blank party)" default both vocab modes.
	must("applyGroupCreateTemplate", "the selection-prefill handler is wired")
	must("renderGroupCreateTemplatePreview", "the roster preview render is wired")
	must("submitGroupCreateFromTemplate", "the instantiate routing exists")
	must("context_override", "the edited context copy is sent as context_override")
	must("descr_override", "the edited description copy is sent as descr_override")
	must("if (mirrorSource && $('#group-create-parent').checked) payload.parent = mirrorSource;", "template-based group create can nest under the mirrored source")
	must("(blank party)", "the regular blank-party default option")
	must("(no circle — a blank party)", "the wizard blank-party default option")

	// (d) The Groups cog's "⎘ from template" shortcut now opens THIS dialog with a
	// circle preselected instead of the separate instantiate modal.
	must("openGroupCreateModal(templates[0].name)", "the cog shortcut opens Form-a-party preselected")
}
