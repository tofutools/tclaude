package agentd

import (
	"regexp"
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
		"pr.updated", "pr.merged", "ci.failed", "ci.succeeded",
		"agent.idle", "agent.awaiting_input", "Sustained for (seconds)",
		"Debounce then delays firing after the dwell matures",
		"{{event.source}}", "{{event.previous_state}}", "{{event.current_state}}",
		"{{agent.id}}", "{{agent.harness}}", "{{event.fact_result}}", "{{event.fact_observed_at}}", "{{event.dwell_started_at}}",
		"Unknown means the fact could not be observed", "selected fact agent", "trigger-harness-capabilities",
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
		"/api/triggers", "/firings?limit=20", "loadTriggerDetail", "row_version=${encodeURIComponent(rule.row_version)}",
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
		".trigger-inspector-grid", ".trigger-editor-step", ".trigger-permission-warning", ".trigger-firing-context",
		".trigger-dwell-states", ".trigger-fact-result.unknown", ".trigger-harness-capabilities",
		".group-triggers-section", "grid-template-columns: 1fr",
		"#trigger-modal .trigger-modal :is(input:not([type=checkbox]):not([type=radio]), select, textarea)",
		"body.wizard #trigger-modal .trigger-modal",
		"body.wizard :is(.trigger-verdicts, .trigger-firings, .group-triggers-section)",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("trigger CSS missing %q", want)
		}
	}
	// Trigger steps are semantic <section>s, while the dashboard's page tabs
	// use a global `section { display: none; }` rule. Keep the scoped override
	// explicit: DOM-only Preact tests still see hidden elements as text.
	triggerStepRule := regexp.MustCompile(`(?s)\.trigger-editor-step\s*\{[^}]*display:\s*block\s*;`)
	if !strings.Contains(triggerUI, "<section class=${`trigger-editor-step") {
		t.Error("trigger editor steps must remain semantic sections")
	}
	if !triggerStepRule.MatchString(css) {
		t.Error("trigger editor steps must override the page-level hidden section rule")
	}
}
