package agentd

import (
	"strings"
	"testing"
)

// The Jobs feature owns both the cron draft and its target picker. This
// source-shape guard pins the scoped-launch contract; component/model tests
// cover the interactive behavior.
//
// The contract: when the cron modal is opened from a group header's
// "⏰ multicast" button, the shared solo/group target picker is scoped
// to that group — its selection cannot leave the group. The group
// dropdown locks to the one group; Solo mode swaps the all-agents
// free-text input for a <select> of only that group's members. Both
// modes (whole-group multicast AND a single member) stay available.
// Non-group entry points (the global "+ new cron job", a per-member
// ⏰, editing an existing job) pass no scopeGroup and are unchanged.
func TestDashboardHTML_CronGroupScopedTargetPicker(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	must("const scope = value.scopeGroup || ''",
		"the Jobs target root derives its group scope from component state")
	must("const members = scope ? groupMembers(snapshot, scope) : []",
		"scoped solo options come only from the scoped group's live members")
	must("disabled=${!!scope}",
		"the group dropdown is locked while scoped")
	must("value=${value.target}",
		"the scoped member selection remains controlled")

	// The native Groups header's "⏰ multicast" button arms the scope; Jobs
	// state preserves the launcher's scopeGroup in the immutable prefill.
	must("scopeGroup: name",
		"the group header cron button must pass scopeGroup")
	must("prefill: { ...prefill }",
		"Jobs state snapshots the launch prefill without writing static DOM")

	// Closing the modal must clear the scope — otherwise a group scope
	// armed by one open would leak into the next (e.g. a global "+ new
	// cron job" opened right after a group multicast).
	must("dialog.value = null",
		"closing the Jobs dialog drops the scoped descriptor")
	if strings.Contains(dashboardAssets, "function bindTargetPicker(") ||
		strings.Contains(dashboardAssets, "function populateTargetPicker(") ||
		strings.Contains(dashboardAssets, "configureCronTargetPicker") {
		t.Error("legacy cron still owns a shared target-picker writer")
	}
}
