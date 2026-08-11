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
	css := dashboardAssetFile(t, "dashboard.css")

	for _, want := range []string{
		"Rules for ${group}",
		"Decrees of party ${group}",
		"Choose from the standing-order library",
		"Consult the archive of decrees",
		"group-standing-orders-summary",
		"group-standing-orders-library-callout",
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("standing-order rulebook dialog missing %q", want)
		}
	}

	if !strings.Contains(actions, "openStandingOrderCreateModal({") ||
		!strings.Contains(actions, "targetMode: 'group', groupName: name, scopeGroup: name") {
		t.Error("group rulebook create action must launch a group-prefilled standing-order editor")
	}
	if !strings.Contains(controller, "openStandingOrderCreateModal") ||
		!strings.Contains(jobs, "openStandingOrderCreate: state.openStandingOrderCreate") {
		t.Error("Jobs controller does not expose standing-order creation to the Groups rulebook")
	}
	for _, want := range []string{
		".group-standing-orders-summary {",
		".group-standing-orders-library-callout {",
		"body.slop .group-standing-orders-live",
		"@media (max-width: 720px)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("standing-order rulebook CSS missing %q", want)
		}
	}
}
