package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_PartyProfilePicker pins the JOH-356 "Form a party" party-
// profile picker in the embedded dashboard source: the create-group dialog can
// start from a template / summoning circle. The wiring lands across HTML (the
// dropdown, the manage affordance, the Task row + roster preview), CSS (the
// wizard skin for the new <select> + preview) and JS (prefill, preview and
// instantiate routing with context_override), in BOTH vocab modes. All
// client-side, so — like the other dashboard render guards — this
// string-searches the embedded source rather than running the JS.
func TestDashboardHTML_PartyProfilePicker(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source still contains %q (%s)", needle, why)
		}
	}

	// Preact has the sole visual owner. dashboard.html provides only the stable
	// host; external surfaces enter through a controller registered at mount.
	must(`<div id="group-create-root"></div>`, "the Preact owner has a stable host")
	must(`hosts: { root: '#group-create-root' }`, "the lazy feature claims the stable host")
	must(`registerGroupCreateController(controller)`, "the island publishes its stable controller")
	mustNot(`<div class="modal-overlay" id="group-create-modal">`, "the legacy static overlay is removed")
	mustNot(`function bindGroupCreateModal`, "the legacy group-create DOM binder is removed")

	// (a) HTML: the party-profile dropdown, its manage affordance, the template-
	// only Task row + roster preview row, and the id the Max-members row needs so
	// it can be hidden in template mode.
	must(`id="group-create-template"`, "the party-profile dropdown exists")
	must(`id="group-create-manage-templates"`, "the ⧉ manage-circles affordance exists")
	must(`plain="Party profile" wizard="Summoning circle"`, "the picker label swaps per vocab mode")
	must(`plain="⧉ manage templates…" wizard="⧉ manage circles…"`, "the manage button label swaps per vocab mode")
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
	must("selectGroupCreateTemplate", "the pure selection-prefill transition is wired")
	must("TemplatePreview", "the Preact roster preview is wired")
	must("actions.submit(draft, template, current.parentGroup)", "the shared action boundary owns create/instantiate routing")
	must("combineGroupAndTemplateContext(", "mirrored template creates combine source-group and template context")
	must("## Mirrored group context", "the combined context labels the mirrored group portion")
	must("## Template context", "the combined context labels the template portion")
	must("context_override", "the edited context copy is sent as context_override")
	must("descr_override", "the edited description copy is sent as descr_override")
	must("else if (draft.source && draft.nested) body.parent = draft.source;", "template-based group create can nest under the mirrored source")
	must("openTemplateManager({ onClose });", "template-manager reconciliation uses an explicit close callback")
	must("state.isCurrent(generation)", "async returns are guarded by the open generation")
	must("if (submitLock.current) return;", "duplicate submissions are synchronously blocked")
	must("(blank party)", "the regular blank-party default option")
	must("(no circle — a blank party)", "the wizard blank-party default option")

	// Group creation has one primary entry point. Templates are selected inside
	// that dialog rather than duplicated as a shortcut in the Groups cog menu.
	if strings.Contains(dashboardAssets, `id="group-from-template-open"`) {
		t.Error("Groups cog still contains the redundant from-template shortcut")
	}
}
