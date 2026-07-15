package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_CronExpressionMode pins the cron dialog's schedule-mode
// toggle (interval chips ⟷ cron expression) and the live explainer wiring.
// Front-end only, so we string-search the embedded source (html + css + js)
// rather than running the JS — a renamed id or a dropped fetch would
// otherwise only fail in the browser.
func TestDashboardHTML_CronExpressionMode(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The mode radios + the two schedule bodies the JS toggles between.
	must(`name="cron-create-schedule-mode"`, "the schedule-mode radio group")
	must(`id="cron-create-schedule-interval"`, "the interval-mode body (chips + custom input)")
	must(`id="cron-create-schedule-cron"`, "the expression-mode body")
	must(`id="cron-create-cron"`, "the cron expression input")
	must(`id="cron-create-cron-explain"`, "the explainer box under the input")

	// The debounced explainer round-trip: the JS posts the expression, the
	// box renders description / next fires / the inline parse error.
	must("explainCron: (expr) => requestMutation('/api/cron/explain'", "DOM-free Jobs explain transport")
	must("const timer = setTimeout(async () => {", "the component-owned debounce wrapper")
	must("firstExplain.current ? 0 : 350", "stored expressions explain immediately and typed expressions debounce")
	must("cron-explain-error", "the inline invalid-expression style hook")

	// The explainer box collapses when empty (like .cron-create-error), so
	// interval-mode saves aren't pushed down by a phantom row.
	must(".cron-explain:empty { display: none; }", "empty explainer collapses")

	// The Jobs tab's info cell renders the expression (with the English
	// description as hover title) instead of "every …" for expression jobs.
	must("cron: ${job.cron_expr}", "the Jobs tab expression info cell")
}
