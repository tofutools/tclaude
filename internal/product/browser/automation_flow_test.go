package browser

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserAutomationScheduleToggleRunAndRecipientHistory(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "recipient", "name": "Scheduled recipient", "desired": model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster .name", "Scheduled recipient")
	page.MustElement("[data-tab=automation]").MustClick()
	page.MustElementR("#automation-list button", "^New schedule$").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Review reminder")
	page.MustElement("#editor [name=anchor]").MustInput(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	page.MustElement("#editor [name=body]").MustInput("Please review the current change.")
	page.MustElement("#editor [name=recipients]").MustSelect("Scheduled recipient")
	page.MustElement("#editor [name=allowed_actions]").MustSelect("message.send")
	page.MustElement("#editor [name=allowed_resources]").MustSelect("Agent: Scheduled recipient")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#automation-list h2", "Review reminder")
	var rules []model.AutomationRule
	require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules", nil, &rules))
	require.Len(t, rules, 1)
	require.False(t, rules[0].Enabled)
	page.MustElementR("#automation-list button", "^Enable$").MustClick()
	page.MustElementR("#automation-list button", "^Disable$").MustClick()
	page.MustElementR("#automation-list button", "^Enable$").MustClick()
	page.MustElementR("#automation-list button", "^Run now$").MustClick()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var records []app.OccurrenceResult
		err := operator.Call(ctx, "GET", "/v2/automation/occurrences?rule_id="+string(rules[0].ID), nil, &records)
		if !assert.NoError(c, err) {
			return
		}
		for _, record := range records {
			if strings.HasPrefix(record.Occurrence.SourceOccurrenceKey, "manual:browser:") && len(record.Occurrence.Recipients) == 1 && record.Occurrence.Recipients[0].Disposition == model.RecipientQueued {
				return
			}
		}
		require.Fail(c, "manual occurrence not queued", "%+v", records)
	}, 15*time.Second, 100*time.Millisecond)
	page.MustElementR("#automation-list button", "^Occurrence history$").MustClick()
	page.MustElementR("#automation-list .occurrence-history", "Scheduled recipient: queued")
	page.MustElementR("#automation-list button", "^Edit rule$").MustClick()
	require.False(t, page.MustHas("#editor [name=enabled]"))
	require.Equal(t, "recipient", page.MustElement("#editor [name=recipients]").MustProperty("value").Str())
	require.Equal(t, "message.send", page.MustElement("#editor [name=allowed_actions]").MustProperty("value").Str())
	page.MustElement("#editor [name=body]").MustSelectAllText().MustInput("Please review the updated change.")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var updated app.AutomationRuleResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules/"+string(rules[0].ID), nil, &updated))
	require.Equal(t, []model.AgentID{"recipient"}, updated.Revision.Action.Message.AgentIDs)
	require.Equal(t, []model.Action{model.ActionSendMessage}, updated.Revision.Delegation.Actions)
	require.Equal(t, "Please review the updated change.", updated.Revision.Action.Message.Body)
	page.MustElementR("#automation-list button", "^Edit rule$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var unchanged app.AutomationRuleResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules/"+string(rules[0].ID), nil, &unchanged))
	require.Equal(t, updated.Rule.HeadRevisionID, unchanged.Rule.HeadRevisionID)
	visibleID := page.MustElement("#automation-list [aria-label=\"Automation rule ID\"]").MustText()
	page.MustElementR("#automation-list button", "^Archive rule$").MustClick()
	require.Contains(t, page.MustElement("#editor").MustText(), visibleID)
	page.MustElement("#editor [name=confirm]").MustInput(visibleID)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	require.False(t, page.MustHas("#automation-list [data-rule]"))
	page.MustElement("[aria-label='Automation state']").MustSelect("archived")
	page.MustElementR("#automation-list button", "^Filter$").MustClick()
	page.MustElementR("#automation-list button", "^Restore rule$")
	require.NotContains(t, page.MustElement("#automation-list").MustText(), "Run now")
	page.MustElementR("#automation-list button", "^Occurrence history$").MustClick()
	page.MustElementR("#automation-list .occurrence-history", "Scheduled recipient: queued")
	page.MustElementR("#automation-list button", "^Restore rule$").MustClick()
	page.MustElement("#editor [name=confirm]").MustInput(visibleID)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustWait(`() => !document.querySelector('#editor').open`)
	page.MustElement("[aria-label='Automation state']").MustSelect("Active catalog")
	page.MustElementR("#automation-list button", "^Filter$").MustClick()
	page.MustElementR("#automation-list button", "^Enable$")
	var restored app.AutomationRuleResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules/"+string(rules[0].ID), nil, &restored))
	require.False(t, restored.Rule.Enabled)
	require.Equal(t, unchanged.Rule.HeadRevisionID, restored.Rule.HeadRevisionID)

	require.Equal(t, updated.Rule.Revision, unchanged.Rule.Revision)
	require.False(t, page.MustElement("#error").MustVisible())
}

func TestBrowserAutomationAuthorsTriggerAndStandingOrder(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "worker", "name": "Review worker", "desired": model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster .name", "Review worker")
	page.MustElement("[data-tab=automation]").MustClick()
	for _, kind := range []string{"trigger", "standing order"} {
		page.MustElementR("#automation-list button", "^New "+kind+"$").MustClick()
		page.MustElement("#editor [name=name]").MustInput("Review " + kind)
		page.MustElement("#editor [name=body]").MustInput("Review the change carefully.")
		page.MustElement("#editor [name=recipients]").MustSelect("Review worker")
		if kind == "trigger" {
			page.MustElement("#editor [name=resource_id]").MustInput("worker")
			page.MustElement("#editor [name=dwell]").MustSelectAllText().MustInput("30")
			page.MustElement("#editor [name=allowed_actions]").MustSelect("message.send")
		} else {
			page.MustElement("#editor [name=pattern]").MustInput("review")
			page.MustElement("#editor [name=allowed_actions]").MustSelect("execution.interact")
		}
		page.MustElement("#editor [name=allowed_resources]").MustSelect("Agent: Review worker")
		page.MustElement("#editor button[type=submit]").MustClick()
		page.MustElement("#editor").MustWaitInvisible()
		page.MustElementR("#automation-list h2", "Review "+kind)
	}
	var rules []model.AutomationRule
	require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules", nil, &rules))
	require.Len(t, rules, 2)
	for _, rule := range rules {
		var read app.AutomationRuleResult
		require.NoError(t, operator.Call(ctx, "GET", "/v2/automation/rules/"+string(rule.ID), nil, &read))
		require.False(t, rule.Enabled)
		require.Equal(t, []model.AgentID{"worker"}, read.Revision.Action.Message.AgentIDs)
		if read.Revision.Condition.Trigger != nil {
			require.Equal(t, 30*time.Second, read.Revision.Condition.Trigger.Dwell)
			require.Equal(t, "worker", read.Revision.Condition.Trigger.Resource.ID)
		} else {
			require.Equal(t, model.StandingOrderSameContinuation, read.Revision.Condition.StandingOrder.Timing)
			require.Equal(t, "review", read.Revision.Condition.StandingOrder.Pattern)
		}
	}
	require.False(t, page.MustElement("#error").MustVisible())
}
