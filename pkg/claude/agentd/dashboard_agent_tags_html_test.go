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

	// helpers.js: the chip + description-cell renderers.
	must("function tagChips(", "tag chips renderer")
	must("function descrCell(", "the Description cell renderer (text + chips + click-to-edit)")
	must("agent-tag-tf", "the tf:<template> task-force chip gets a distinct class")
	must(`data-act="edit-descr"`, "the descr cell is click-to-edit")
	must("descr-add", "the empty descr+tags cell shows a discoverable hint")

	// render.js: the descr cell renders through descrCell (not raw esc'd text).
	must("descrCell(m, ctx.group)", "the descr column renders through descrCell")
	must("descrCell,", "render.js imports descrCell from helpers")

	// row-actions.js: the descr entry point + the independent tags write.
	must("case 'edit-descr':", "the descr cell opens the edit-member modal")
	must("/api/agents/${encodeURIComponent(agent)}/tags", "the tags write hits the agent-tags endpoint")
	must("'tags' in result", "the tags edit is applied independently of the role/descr PATCH")

	// refresh.js: the modal Tags field + its parse/compare.
	must("#edit-member-tags", "the edit-member modal has a Tags field")
	must("function parseTagsField(", "the Tags field is parsed into a de-duped set")
	must("function sameTagSet(", "a set-equal Tags edit is not written")

	// dashboard.html: the Tags input in the edit-member modal.
	must(`id="edit-member-tags"`, "the Tags input is present in the modal")

	// dashboard.css: the chip styling in BOTH the default and wizard themes
	// (the operator asked wizard mode not be left unstyled), and the wizard
	// recolor stays scoped to the chip (no unscoped body.wizard widening).
	must(".agent-tag {", "the tag chips have a default-theme rule")
	must(".agent-tag-tf {", "the task-force chip has its own colour")
	must("body.wizard .agent-tag {", "wizard mode themes the tag chips (scoped to .agent-tag)")
}
