package agentd

import (
	"strings"
	"testing"
)

// Agent rename used to be a standalone "rename" button + its own modal.
// It folded into two surfaces, both of which POST the SAME request to
// /api/agents/{conv}/rename:
//   - the per-agent edit panel (the "edit" button → editMemberModal),
//     which gained a Title field and an "auto" self-rename checkbox;
//   - the click-to-edit agent-name cell (the .rowname-text span → a keyed
//     native InlineEditor owned by the Groups interaction provider).
//
// The change is entirely in the embedded dashboard JS/HTML, so no
// server path a flow test can reach proves the WIRING. This guards
// that shape: the old standalone modal stays gone, and both new
// surfaces stay wired to the rename endpoint. dashboard_rename_flow_test.go
// is the companion that exercises the endpoint itself.
func TestDashboardRenameUI_FoldedIntoEditAndNameCell(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	absent := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still contain %q (%s)", needle, why)
		}
	}

	// The standalone rename modal and its button are gone — rename is
	// no longer a separate path.
	absent(`id="rename-agent-modal"`, "the standalone rename modal was removed")
	absent("bindRenameAgentModal", "the standalone rename modal binding was removed")
	absent("openRenameAgentModal", "the standalone rename modal opener was removed")
	absent(`data-act="rename-agent"`, "the standalone rename button action was removed")
	absent("function renameAgentButton", "the standalone rename button renderer was removed")

	// Change 1 — rename folded into the per-agent edit panel: the edit
	// modal gained a Title input and an "auto" self-rename checkbox.
	present(`act="edit-member"`, "the per-agent edit button is still wired")
	present(`id="edit-member-title-input"`, "the edit panel has a Title field")
	present(`id="edit-member-auto"`, "the edit panel has the auto self-rename checkbox")

	// Change 2 — the agent-name cell is click-to-edit, routed through the keyed
	// native editor; row DnD is disabled only while that editor is active.
	present(`'data-act': 'rename-name'`, "the agent-name cell is click-to-edit")
	present(`class: 'rowname-text'`, "the agent name renders as a click-to-edit span")
	present("<${InlineEditor}", "the shared native inline-editor component is used")
	present("draggable=${interactions.editorKey !== editorKey}", "name editing preserves the DnD boundary")

	// Both new surfaces POST to the one rename endpoint — the edit
	// panel's Save and the inline name handler issue this exact fetch.
	present("`/api/agents/${encodeURIComponent(agent)}/rename`",
		"a rename surface POSTs to /api/agents/{agent}/rename")
}
