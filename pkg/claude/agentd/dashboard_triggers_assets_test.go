package agentd

import (
	"strings"
	"testing"
)

func TestDashboardTriggersAssets(t *testing.T) {
	triggerUI := dashboardAssetFile(t, "js/jobs-triggers.js")
	jobsState := dashboardAssetFile(t, "js/jobs-state.js")
	jobsActions := dashboardAssetFile(t, "js/jobs-actions.js")
	groups := dashboardAssetFile(t, "js/groups-list.js")
	css := dashboardAssetFile(t, "dashboard.css")

	for _, want := range []string{
		"TriggerWorkspace", "TriggerInspector", "TriggerDialogRoot",
		"WHEN", "WHERE", "THEN", "trigger-permission-warning",
		"cron-create-modal trigger-modal",
		"{{pr.url}}", "{{pr.number}}", "{{pr.branch}}", "{{pr.author_agent}}", "{{group}}",
		"The current API does not expose a live fact snapshot",
	} {
		if !strings.Contains(triggerUI, want) {
			t.Errorf("trigger dashboard UI missing %q", want)
		}
	}
	for _, want := range []string{"trigger: 'triggers'", "openTriggerCreate", "openTriggerEdit", "invalidateTriggers"} {
		if !strings.Contains(jobsState, want) {
			t.Errorf("trigger navigation/state missing %q", want)
		}
	}
	for _, want := range []string{
		"/api/triggers", "/firings?limit=20", "row_version=${encodeURIComponent(rule.row_version)}",
	} {
		if !strings.Contains(jobsActions, want) {
			t.Errorf("trigger REST action wiring missing %q", want)
		}
	}
	for _, want := range []string{
		"group-triggers-section", "rows.slice(0, 5)", "edit in Automations",
	} {
		if !strings.Contains(groups, want) {
			t.Errorf("compact group projection missing %q", want)
		}
	}
	for _, want := range []string{
		".trigger-inspector-grid", ".trigger-editor-step", ".trigger-permission-warning",
		".group-triggers-section", "grid-template-columns: 1fr",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("trigger CSS missing %q", want)
		}
	}
}
