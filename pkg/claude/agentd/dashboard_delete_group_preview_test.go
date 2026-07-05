package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_DeleteGroupPreviewWired pins the richer delete-group
// dialog. The dashboard path is frontend orchestration over existing endpoints:
// preview the current group members, check sole-group agents for retirement by
// default, leave multi-group agents unchecked by default, let the human toggle
// either cohort, then DELETE the group.
func TestDashboardHTML_DeleteGroupPreviewWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	must(`id="delete-group-modal"`, "the delete-group preview overlay exists")
	must(`id="delete-group-list"`, "the preview has a per-agent decision list")
	must(`id="delete-group-retire" checked`, "retiring single-group members defaults on")
	must(`single-group agents are checked by default; agents also in other groups are unchecked by default`,
		"the modal explains the default retire selection")
	must(`Banish checked familiars before disbanding the party`, "wizard copy uses familiar/party wording")
	must(`single-party familiars are checked by default; familiars also in other parties`,
		"wizard helper copy uses familiar/party wording")
	must("function openDeleteGroupModal(group)", "refresh.js defines the shared modal driver")
	must("openDeleteGroupModal,", "refresh.js exports the modal driver")
	must("openDeleteGroupModal(group);", "row-actions delete-group opens the preview")
	must("groupDeletePlan(group)", "the modal builds its preview from the snapshot")
	must("const onlyThisGroup = otherGroups.length === 0", "the plan identifies single-group members")
	must("checked: onlyThisGroup", "single-group agents default checked; multi-group agents default unchecked")
	must(`type="checkbox" data-agent`, "each preview row gets a retirement checkbox")
	must("listEl.addEventListener('change', onListChange)", "per-row checkbox changes are wired")
	must("if (m) m.checked = cb.checked", "per-row toggles update the retirement selection")
	must("not auto-${w.retired}", "multi-group members are visible but not automatically retired/banished")
	must("explicitly included", "checking a multi-group member opts it into retirement")
	must("deleteTitle: wiz ? 'Disband this party?' : 'Delete group'", "wizard title/submit says Disband this party")
	must("Conversation scrolls are kept.", "wizard hint copy keeps the scroll/familiar vocabulary")
	must("retireDecision: wiz ? 'banish familiar + stop' : 'retire + stop'", "wizard row decision says banish familiar")
	must("const toRetire = retireTargets().map(m => m.agent_id || m.conv_id).filter(Boolean);",
		"submit sends only preview-approved retire targets")
	must("JSON.stringify({ convs: toRetire, shutdown: true, delete_worktree: false })",
		"single-group members retire via explicit list with worktrees kept")
	must("method: 'DELETE', credentials: 'same-origin'",
		"the final group delete still runs after any retire step")
}

// TestDashboardJS_GroupDragToDeleteWired guards the drag-to-banish group path:
// group-reorder drags reveal the same fixed #dnd-trash overlay used by member
// drags, and a group drop there opens the delete-group preview instead of
// applying a reorder/nest mutation.
func TestDashboardJS_GroupDragToDeleteWired(t *testing.T) {
	for _, c := range []struct{ needle, why string }{
		{"import { refresh, toast, openDeleteGroupModal } from './refresh.js';",
			"group-reorder imports the shared delete preview"},
		{"function groupTrashTarget(e)", "group-reorder recognises #dnd-trash as a group drop target"},
		{"if (trash) trash.classList.add('show');",
			"group header dragstart reveals the overlay"},
		{"trash.classList.add('dnd-drop-over');",
			"group dragover arms the overlay"},
		{"`↓ delete group ${groupDragName}`",
			"group dragover shows the delete intent pill"},
		{"`↓ disband party ${groupDragName}`",
			"wizard group dragover uses disband-party wording"},
		{"openDeleteGroupModal(dragName);",
			"dropping a group on the overlay opens the delete preview"},
		{"trash.classList.remove('show', 'dnd-drop-over');",
			"endGroupDrag hides and disarms the overlay"},
	} {
		if !strings.Contains(dashboardAssets, c.needle) {
			t.Errorf("dashboard assets missing %q — %s", c.needle, c.why)
		}
	}
}
