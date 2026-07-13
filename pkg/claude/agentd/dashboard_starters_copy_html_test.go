package agentd

import "testing"

// TestDashboardHTML_StartersCopyMechanism pins the JOH-353 "copy, don't cast"
// surface of the ⭐ starters modal in the embedded dashboard source. The
// operator's directive: nobody should click a starter's action expecting agents
// to appear — installing/conjuring a preset makes a COPY into the user's own
// templates/circles list, it does not spawn. That intent has to land in all
// three layers (intro line, per-row button, post-install toast) and in BOTH
// vocab modes, plus the modal must carry the sibling dialogs' wizard skin. All
// of it is client-side HTML/CSS/JS, so — like the other dashboard render guards
// — this string-searches the embedded source rather than running the JS.
func TestDashboardHTML_StartersCopyMechanism(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The ⭐ starters modal stays as the one obvious starters path.
	must(`id: 'starters-modal'`, "the starters modal overlay still exists")
	must(`id="starters-open"`, "the ⭐ starters toolbar button still opens it")
	must("openTemplateStarters", "the Preact modal open action is wired")

	// (a) Intro line leads with the copy-into-your-list fact + not-spawn caveat,
	// both vocab modes.
	must("Copies a ready-made team into your own templates list", "regular intro leads with the copy fact")
	must("does not spawn anything yet", "regular intro says it does not spawn")
	must("Copies a ready-made party into your own circles", "wizard intro leads with the copy fact")
	must("summons no familiars yet", "wizard intro says it summons nothing")

	// (b) Per-row action button reads copy-not-cast, both vocab modes.
	must("⤓ copy to my templates", "the regular install button says copy-to-my-templates")
	must("⭐ copy into my circles", "the wizard install button says copy-into-my-circles")

	// (c) Post-install toast says WHERE it landed and that nothing spawned.
	must("added to your templates:", "the regular toast reports the templates list")
	must("nothing spawned yet", "the regular toast says nothing spawned")

	// Empty-state nudge points at the ⭐ starters path.
	must("No templates yet — use + new template, ⤓ from a group, or ⭐ starters above.", "the empty templates overlay nudges toward starters")

	// (d) Wizard visual skin for the modal — a body.wizard #starters-modal block
	// exists (a white button regression can't be caught by strings, but the
	// block's ABSENCE can).
	must("body.wizard #starters-modal .cron-create-modal {", "the starters modal has a wizard skin block")
	must("body.wizard #starters-modal .starter-row {", "the starter rows are wizard-skinned")
	must("body.wizard #starters-modal .starter-actions button", "the copy button is wizard-skinned")
}
