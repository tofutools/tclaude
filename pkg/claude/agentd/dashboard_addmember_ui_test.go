package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAddMemberUI_PreactOwned pins the browser ownership seam while
// dashboard_addmember_flow_test.go exercises the production membership API.
func TestDashboardAddMemberUI_PreactOwned(t *testing.T) {
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

	present(`id="groups-add-member-dialog-root"`, "the Groups feature has a stable picker host")
	present("mountGroupsAddMemberDialog", "the Groups feature mounts the Preact picker")
	present("function buildAddMemberCandidates(", "candidate union/filtering is component-owned")
	present("loadAddMemberPromotionPool", "the full promotion pool remains asynchronous and retryable")
	present("state.optimisticAddMember", "successful membership writes update the Groups signal immediately")
	present("`/api/groups/${encodeURIComponent(group)}/members`", "the production membership endpoint remains wired")
	present("function openToolbarProfilePicker(", "the unrelated dashboard-toolbar picker stays isolated")
	absent("function addMemberModal(", "the legacy imperative picker lifecycle was removed")
	absent(`<div class="modal-overlay" id="add-member-modal">`, "the static picker form was removed")
	absent("case 'add-member':", "the delegated row-action picker controller was removed")
}
