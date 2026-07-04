package agentd

import (
	"strings"
	"testing"
)

// JOH-393: the REVERSE palette drag — drag a LIVE agent row / group header ONTO
// the palette dock to capture it as a spawn profile / group template. The wiring
// spans dock-save-dnd.js (the drop routing), dashboard.js (bind order), the two
// editors' pre-fill-in-create-mode paths, and dashboard.css (the capture
// highlight). There's no JS runner, so — like the forward-drag guard
// (TestDashboardHTML_TemplateDrop) — this pins the pieces by string-searching
// the embedded source so a rename that silently breaks the drag in the browser
// fails at `go test ./...` instead. The endpoints themselves are covered by
// TestGroupTemplate_FromGroupPreview / TestSpawnProfile_FromAgentSeed.
func TestDashboardHTML_DockSaveDrag(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// dock-save-dnd.js exists and is bound AFTER the two reverse-source modules
	// so its dragover wins the shared pill over the dock.
	must("import { bindDockSaveDnd } from './dock-save-dnd.js';", "dashboard.js imports the reverse-drag binder")
	must("bindDockSaveDnd();", "dashboard.js wires the reverse drag in")
	must("export { bindDockSaveDnd };", "dock-save-dnd exports its binder")

	// It reuses the two EXISTING drag sources' MIMEs + active flags — no new
	// drag source.
	must("import { dndDragActive } from './dnd.js';", "reads the member-drag active flag")
	must("import { groupReorderActive } from './group-reorder.js';", "reads the group-drag active flag")
	must("const MEMBER_MIME = 'application/x-tclaude-member';", "keys off the member payload MIME")
	must("const GROUP_MIME = 'application/x-tclaude-group';", "keys off the group payload MIME")

	// The dock (open panel + collapsed edge tab) is the drop target.
	must("const DOCK_TARGET_SEL = '#agent-dock, #dock-toggle';", "the dock panel + edge tab are the drop target")

	// The drop routes to the two seed captures and opens the editors pre-filled
	// but UNSAVED (create mode), so the human previews before Save.
	must("/api/spawn-profiles/from-agent", "an agent drop captures a profile seed")
	must("openProfileEditor(seed, { editExisting: false })", "an agent drop opens the profile editor in create mode")
	must("preview: true", "a group drop captures a non-persisting template preview")
	must("openTemplateEditor(tmpl, { asNew: true })", "a group drop opens the template editor in create mode")

	// The template editor's create-mode-with-prefill option (asNew) exists.
	must("function openTemplateEditor(tmpl, { asNew = false } = {}) {", "openTemplateEditor gained the asNew option")
	must("const editing = tmpl && !asNew;", "asNew keeps the editor in create mode while pre-filling")

	// The capture highlight (a DISTINCT class from the forward drag's
	// .dock-drop-over) + its CSS.
	must("clearDockSaveHighlight", "the reverse drag owns a distinct highlight")
	must("#agent-dock.dock-save-over .dock-inner {", "the open dock glows on a reverse-drag hover")
	must("#dock-toggle.dock-save-over {", "the collapsed dock's edge tab also accepts the save")
}
