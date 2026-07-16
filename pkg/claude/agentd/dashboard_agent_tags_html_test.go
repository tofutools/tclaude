package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_AgentTagsWired pins the browser-side wiring of the agent
// tags feature (JOH-380). The tag chips + click-to-edit descr cell live only in
// the embedded dashboard assets, where no Go path exercises them; asserting on
// the embedded concatenation catches a drop / rename at `go test ./...`. (The
// backend is covered by tags_flow_test.go.)
func TestDashboardHTML_AgentTagsWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// groups-member-table.js: the native chip + description-cell components.
	must("function TagChips(", "tag chips component")
	must("function DescrCell(", "the Description cell component (text + chips + click-to-edit)")
	must("agent-tag-tf", "the tf:<template> task-force chip gets a distinct class")
	must("editableMemberCellAttrs(member, group, actions, 'edit-descr', 'descr')", "the descr cell directly opens the Preact editor")
	must("descr-add", "the empty descr+tags cell shows a discoverable hint")

	// The native column maps through DescrCell rather than raw text.
	must("<${DescrCell} member=${member} group=${group} actions=${actions} />", "the descr column renders through DescrCell")
	must("case 'descr':", "the member-column switch owns the description cell")

	// member-editor-actions.js: tags persist independently from membership edits.
	must("/api/agents/${encodeURIComponent(descriptor.agent)}/tags", "the tags write hits the agent-tags endpoint")
	must("if ('tags' in changes)", "the tags edit is applied independently of the role/descr PATCH")

	// member-editor-island.js: the Preact-owned Tags field + parse/compare.
	must(`id="edit-member-tags"`, "the edit-member modal has a Tags field")
	must("function parseMemberTags(", "the Tags field is parsed into a de-duped set")
	must("function sameTagSet(", "a set-equal Tags edit is not written")

	// dashboard.html retains only the stable Preact host, not a second form owner.
	must(`id="groups-member-dialog-root"`, "the member editor has a stable Preact host")

	// dashboard.css: the chip styling in BOTH the default and wizard themes
	// (the operator asked wizard mode not be left unstyled), and the wizard
	// recolor stays scoped to the chip (no unscoped body.wizard widening).
	must(".agent-tag {", "the tag chips have a default-theme rule")
	must(".agent-tag-tf {", "the task-force chip has its own colour")
	must("body.wizard .agent-tag {", "wizard mode themes the tag chips (scoped to .agent-tag)")
}
