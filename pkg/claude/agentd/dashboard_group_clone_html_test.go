package agentd

import "testing"

// TestDashboardHTML_GroupCloneModal pins the clone-group modal wiring in
// the embedded dashboard source. Clicking ⧉ clone… in a group's ⚙ cog
// menu opens the Preact-owned parameters modal: an editable new-name (prefilled
// with the computed <source>-c-N default), clone-scope checkboxes, and a live
// preview of every setting the clone will carry.
func TestDashboardHTML_GroupCloneModal(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	// The cog-menu trigger + its dispatch.
	must(`data-act="clone-group"`, "the group cog menu carries a clone button")
	must("case 'clone-group':", "row-actions.js dispatches the clone-group action")
	must("openGroupCloneModal(group)", "the dispatcher opens the clone modal")

	// The modal shell + its controls.
	must(`id: 'group-clone-modal'`, "the clone modal overlay exists")
	must(`id="group-clone-name"`, "the modal has an editable new-name field")
	must(`id="group-clone-with-agents"`, "the modal has a clone-agents checkbox")
	must(`id="group-clone-copy-owners"`, "the modal has an explicit copy-owners checkbox")
	must(`id="group-clone-preview"`, "the modal has a settings preview panel")
	must(`id="group-clone-submit"`, "the modal has a submit button")
	// The Preact-controlled `checked=${withAgents}` attribute is dynamic, so its
	// unchecked initial value is asserted behaviorally in the JS component test.

	// The JS behaviour: default-name computation, preview render, and the POST.
	must("defaultName: `${prefix}${suffix}`", "client computes the <source>-c-N default name")
	must("function GroupClonePreview(", "the Preact modal renders a settings preview")
	must("no_clone_members", "submit sends the with/without-agents flag")
	must("copy_owners", "submit sends the owner-copy opt-in flag")
	must("/clone`", "submit POSTs to the group clone endpoint")

	// The preview's CSS ships with the page.
	must(".group-clone-preview {", "the preview panel has a CSS rule")
}
