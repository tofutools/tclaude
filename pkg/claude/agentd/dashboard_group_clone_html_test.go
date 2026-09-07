package agentd

import "testing"

// TestDashboardHTML_GroupCloneModal pins clone-group wiring into the shared
// group-creation modal. Clicking ⧉ clone… in a group's ⚙ cog menu opens
// the same source-first Preact form as every other group-create entry point,
// prefilled with the source, placement, next clone name, clone-scope controls,
// and a live preview of every setting the clone will carry.
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

	// The shared modal shell + clone-only controls.
	must(`id="group-create-modal"`, "the shared group creation overlay exists")
	must(`id="group-create-name"`, "the shared modal has an editable name field")
	must(`id="group-create-with-agents"`, "clone mode has a clone-agents checkbox")
	must(`id="group-create-copy-owners"`, "clone mode has an explicit copy-owners checkbox")
	must(`id="group-create-attachment-url"`, "the shared modal has an editable attachment link")
	must(`id="group-create-attachment-label"`, "the shared modal has an editable attachment label")
	must(`id="group-create-environment-row"`, "the shared modal has editable group environment variables")
	must(`id="group-create-source-summary"`, "the shared modal has a source summary panel")
	must(`class="group-create-origin-options"`, "the shared modal exposes the source-first selector")
	must(`id="group-create-group-source"`, "the source group is visibly selectable")
	must(`id="group-create-placement"`, "placement is editable in every create flow")
	must(`id="group-create-submit"`, "clone mode uses the shared create submit button")
	// The Preact-controlled `checked=${withAgents}` attribute is dynamic, so its
	// unchecked initial value is asserted behaviorally in the JS component test.

	// The JS behaviour: default-name computation, preview render, and the POST.
	must("defaultName: nextGroupCloneName(groups, groupName)", "client computes the <source>-c-N default name")
	must("function GroupSourceSummary(", "the shared Preact modal renders a source summary")
	must("source.attachment_url", "the preview discloses the attachment copied with group settings")
	must("source.environment", "the preview discloses the environment copied with group settings")
	must("no_clone_members", "submit sends the with/without-agents flag")
	must("copy_owners", "submit sends the owner-copy opt-in flag")
	must("/clone`", "shared submit POSTs to the group clone endpoint")

	// The preview's CSS ships with the page.
	must(".group-clone-preview {", "the preview panel has a CSS rule")
}
