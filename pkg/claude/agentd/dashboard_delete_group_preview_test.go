package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardHTML_DeleteGroupPreviewWired pins the richer delete-group
// dialog. The dashboard path is frontend orchestration over existing endpoints:
// preview the current group members, check sole-group agents for retirement by
// default, leave multi-group agents unchecked by default, let the human toggle
// either cohort, then DELETE the group.
func TestDashboardHTML_DeleteGroupPreviewWired(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	html := read("dashboard.html")
	refresh := read("js/refresh.js")
	controller := read("js/transaction-dialog-controller.js")
	island := read("js/transaction-dialog-island.js")
	actions := read("js/transaction-dialog-actions.js")

	if strings.Contains(html, `id="delete-group-modal"`) {
		t.Error("static dashboard HTML still owns #delete-group-modal")
	}
	for source, required := range map[string][]string{
		controller: {
			"buildDeleteGroupDescriptor(snapshot, groupName)",
			"openDeleteGroupDialog(snapshot, group)",
			"memberships,", "otherGroups,", "defaultRetire: otherGroups.length === 0",
		},
		island: {
			"kind === 'delete-group'", `id="delete-group-modal"`, `id="delete-group-list"`,
			`id="delete-group-retire"`, "explicitly included", "not auto-retired",
			"Banish checked familiars before disbanding the party",
			"single-party familiars are checked by default; familiars also in other parties",
		},
		actions: {
			"async deleteGroupPlan(request)", "pendingRetire", "retireComplete",
			"deleteComplete", "memberErrors", "method: 'DELETE'",
		},
		refresh: {
			"function openDeleteGroupModal(group)",
			"return openDeleteGroupDialog(lastSnapshot, group)", "openDeleteGroupModal,",
		},
	} {
		for _, needle := range required {
			if !strings.Contains(source, needle) {
				t.Errorf("delete-group Preact ownership contract missing %q", needle)
			}
		}
	}
	if !strings.Contains(dashboardAssets, "openDeleteGroupModal(group);") {
		t.Error("row-actions delete-group no longer opens the shared snapshot launcher")
	}
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
