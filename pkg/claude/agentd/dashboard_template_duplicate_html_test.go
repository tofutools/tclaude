package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_TemplateDuplicate pins the JOH-365 "⧉ duplicate a template"
// surface in the embedded dashboard source: a per-card duplicate action that
// asks for the copy's name and re-POSTs the source template's own JSON to the
// create endpoint (full-fidelity clone, no dedicated backend). Like the other
// dashboard render guards, it string-searches the embedded HTML/CSS/JS rather
// than running the browser — the wiring spans three files that must stay in
// lockstep, and both vocab modes (regular + 🧙 wizard) have to carry the copy.
func TestDashboardHTML_TemplateDuplicate(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// (a) The per-card action button — routed by data-tact (NOT data-act, which
	// keeps it off the global row-action bus), with both vocab labels.
	must(`data-tact="duplicate"`, "the template card carries the duplicate action")
	must("⧉ duplicate", "the regular card button reads ⧉ duplicate")
	must("🪞 mirror", "the wizard card button reads 🪞 mirror")

	// (b) The delegated click handler routes duplicate to the name dialog.
	must("btn.dataset.tact === 'duplicate'", "the delegated handler routes the duplicate action")
	must("openDuplicateModal", "the duplicate dialog open handler is wired")
	must("submitDuplicate", "the duplicate submit handler is wired")

	// (c) The name dialog — ids + both vocab titles. It prefills <name>-copy and
	// re-POSTs to the create endpoint (the 409 there is the collision guard).
	must(`id="template-duplicate-modal"`, "the duplicate dialog overlay ships")
	must(`id="template-duplicate-name"`, "the duplicate name input ships")
	must(`id="template-duplicate-submit"`, "the duplicate submit button ships")
	must(`id="template-duplicate-source"`, "the dialog names the source template")
	must("Duplicate a template", "the regular dialog title ships")
	must("🪞 Mirror the circle", "the wizard dialog title ships")
	must("`${name}-copy`", "the dialog prefills the copy name with a -copy suffix")

	// (d) The clone re-POSTs the source template's own JSON (name swapped) to the
	// shared create endpoint — the fidelity contract this UI depends on.
	must("const payload = { ...src, name };", "duplicate clones the source template JSON with the name swapped")
	must("delete payload.created_at;", "duplicate drops the response-only created_at before re-POSTing")
	must("delete payload.updated_at;", "duplicate drops the response-only updated_at before re-POSTing")
	must("template duplicated:", "the success toast reports the new template")

	// (e) Plain-mode chrome for the new card button comes from the shared base
	// `button.tool { … }` rule (JOH-364) — the button carries the .tool class, so
	// no bespoke skin is needed. Pin that it is a .tool button (rides the base
	// rule) rather than pinning a now-removed scoped block. The dialog's wizard
	// skin block is still bespoke (a white-button regression can't be caught by
	// strings, but the block's ABSENCE can).
	must(`class="tool" data-tact="duplicate"`, "the duplicate card button is a .tool button, so it rides the shared base button.tool skin")
	must("body.wizard #template-duplicate-modal .cron-create-modal {", "the duplicate dialog has a wizard skin block")
	must(`content: "🪞 Mirror it!";`, "the wizard submit lever reads 🪞 Mirror it!")
}
