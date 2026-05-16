package agentd

import (
	"strings"
	"testing"
)

// The cron-create modal's group scoping lives entirely in
// dashboard.html's embedded JS — there is no Go code path to exercise
// (the daemon already accepts both a group target and a member
// conv-id target; nothing server-side changed), and the repo has no
// JS test runner. So this guard pins the *contract* of the scoping so
// a future edit cannot silently regress it.
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
		if !strings.Contains(dashboardHTML, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// The scope state + its arm/clear entry point.
	must("const targetPickerScopes = {}",
		"the per-picker scope registry must exist")
	must("function setTargetPickerScope(prefix, groupName)",
		"setTargetPickerScope arms/clears a picker's group scope")

	// The scoped-mode DOM: a members <select> row distinct from the
	// free-text solo row and the group dropdown.
	must(`id="${prefix}-target-scoped"`,
		"the scoped-solo row must be in the picker markup")
	must(`id="${prefix}-scoped-member"`,
		"the scoped member <select> must be in the picker markup")
	must("function populateTargetPickerMembers(prefix)",
		"the scoped member <select> must be populated from the group's members")

	// The group dropdown locks to the one scoped group — a scoped
	// multicast cannot be retargeted elsewhere.
	must("const groups = scope",
		"populateTargetPickerGroups must lock the dropdown to the scoped group")
	must("sel.disabled = !!scope",
		"the group dropdown is disabled (locked) while scoped")

	// readTargetPicker reads the scoped member <select> in scoped solo
	// mode — so the submitted target is structurally a group member.
	must("$('#' + prefix + '-scoped-member').value.trim()",
		"readTargetPicker must read the scoped member <select> when scoped")

	// The group header's "⏰ multicast" button arms the scope; the
	// shared populateCronForm wires the prefill's scopeGroup through.
	must("scopeGroup: g.name",
		"the group header cron button must pass scopeGroup")
	must("setTargetPickerScope('cron-create', p.scopeGroup)",
		"populateCronForm must arm/clear the scope from the prefill")
}
