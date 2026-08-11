package agentd

import (
	"strings"
	"testing"
)

func TestDashboardGroupStandingOrdersRulebookAssets(t *testing.T) {
	dialog := dashboardAssetFile(t, "js/groups-standing-orders-dialog.js")
	actions := dashboardAssetFile(t, "js/groups-actions.js")
	controller := dashboardAssetFile(t, "js/jobs-controller.js")
	jobs := dashboardAssetFile(t, "js/jobs-island.js")
	jobsState := dashboardAssetFile(t, "js/jobs-state.js")
	orderEditor := dashboardAssetFile(t, "js/jobs-standing-order-dialog-island.js")
	css := dashboardAssetFile(t, "dashboard.css")

	for _, want := range []string{
		"Rules for ${group}",
		"Decrees of party ${group}",
		"Choose from the standing-order library",
		"Consult the archive of decrees",
		"group-standing-orders-summary",
		"group-standing-orders-library-callout",
		"group-standing-orders-load-failed",
		"group-standing-orders-paused",
		"group-standing-orders-first-run",
		"The party bears no decrees",
		"No standing orders yet",
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("standing-order rulebook dialog missing %q", want)
		}
	}

	if !strings.Contains(actions, "openStandingOrderCreateModal({") ||
		!strings.Contains(actions, "targetMode: 'group', groupName: name, scopeGroup: name") ||
		!strings.Contains(actions, "onCancel: () => state.openStandingOrders({ name })") {
		t.Error("group rulebook create action must launch a group-prefilled standing-order editor")
	}
	if !strings.Contains(controller, "openStandingOrderCreateModal") ||
		!strings.Contains(jobs, "openStandingOrderCreate: state.openStandingOrderCreate") {
		t.Error("Jobs controller does not expose standing-order creation to the Groups rulebook")
	}
	if !strings.Contains(jobsState, "if (cancelled && typeof closing?.onCancel === 'function') closing.onCancel()") ||
		!strings.Contains(orderEditor, "const cancel = () => actions.closeStandingOrderDialog({ cancelled: true })") ||
		!strings.Contains(orderEditor, "actions.closeStandingOrderDialog();") {
		t.Error("standing-order cancellation must return to its caller without treating a successful save as cancellation")
	}
	for _, want := range []string{
		".group-standing-orders-summary {",
		".group-standing-orders-library-callout {",
		"body.wizard .group-standing-orders-live",
		"body.wizard #group-standing-orders-modal .cron-create-modal",
		"#group-standing-orders-modal .cron-create-modal button.primary",
		"body.wizard #standing-order-modal .cron-create-modal",
		"body.wizard #standing-order-modal #standing-order-title::before",
		"@media (max-width: 720px)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("standing-order rulebook CSS missing %q", want)
		}
	}
}
