package agentd_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/standingorders"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func mutateStandingOrder(
	t *testing.T, dash http.Handler, method, path string, body any,
) (*standingorders.OrderView, int) {
	t.Helper()
	rec := testharness.Serve(dash, dashReq(t, method, path, body))
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var view standingorders.OrderView
	testharness.DecodeJSON(t, rec, &view)
	return &view, rec.Code
}

func standingOrderBody(target string, rowVersion int64) map[string]any {
	return map[string]any{
		"name": "boundary-reminder", "row_version": rowVersion, "target": target,
		"summary": "Re-read the durable instructions.", "trigger_event": "session.start",
		"sources": []string{"compact", "resume"}, "timing": "same-continuation",
		"cadence": "always", "cooldown_seconds": 60, "enabled": true,
	}
}

func standingOrderActionPath(id int64, action string, rowVersion int64) string {
	path := fmt.Sprintf("/api/standing-orders/%d", id)
	if action != "" {
		path += "/" + action
	}
	return fmt.Sprintf("%s?row_version=%d", path, rowVersion)
}

func TestDashboardStandingOrders_CRUDLifecycleAndRowVersionGuards(t *testing.T) {
	newFlow(t)
	targetAgent, _, err := db.EnsureAgentForConv("conv-standing-target", "test")
	require.NoError(t, err)
	dash := agentd.BuildDashboardHandlerForTest()

	created, code := mutateStandingOrder(t, dash, http.MethodPost,
		"/api/standing-orders", standingOrderBody(targetAgent, 0))
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, created)
	assert.Positive(t, created.ID)
	assert.Equal(t, int64(1), created.Revision)
	assert.Equal(t, int64(1), created.RowVersion)
	assert.Equal(t, targetAgent, created.Target.Agent)
	assert.True(t, created.OperatorAuthored)
	assert.Equal(t, int64(60), created.CooldownSeconds)

	legacyEnable := fmt.Sprintf(
		"/api/standing-orders/%d/enable?revision=%d&updated_at=legacy",
		created.ID, created.Revision,
	)
	rec := testharness.Serve(dash, dashReq(t, http.MethodPost, legacyEnable, nil))
	require.Equal(t, http.StatusNoContent, rec.Code,
		"an already-open pre-migration dashboard may use revision as its initial row token")

	editBody := standingOrderBody(targetAgent, created.RowVersion)
	editBody["name"] = "boundary-reminder-edited"
	editBody["summary"] = "Updated durable instruction."
	editBody["timing"] = "next-turn"
	editBody["match_field"] = "cwd"
	editBody["match_regex"] = `(?i)/release$`
	edited, code := mutateStandingOrder(t, dash, http.MethodPatch,
		fmt.Sprintf("/api/standing-orders/%d", created.ID), editBody)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, edited)
	assert.Equal(t, int64(2), edited.Revision)
	assert.Equal(t, int64(2), edited.RowVersion)
	assert.Equal(t, "boundary-reminder-edited", edited.Name)
	assert.Equal(t, db.StandingTimingNextTurn, edited.Timing)
	assert.Equal(t, db.StandingMatchFieldCwd, edited.Trigger.MatchField)
	assert.Equal(t, `(?i)/release$`, edited.Trigger.MatchRegex)

	_, code = mutateStandingOrder(t, dash, http.MethodPatch,
		fmt.Sprintf("/api/standing-orders/%d", created.ID), editBody)
	assert.Equal(t, http.StatusConflict, code, "a stale editor cannot overwrite a newer row version")

	rec = testharness.Serve(dash, dashReq(t, http.MethodPost,
		standingOrderActionPath(created.ID, "disable", edited.RowVersion), nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	disabled, err := db.GetStandingOrder(created.ID)
	require.NoError(t, err)
	require.NotNil(t, disabled)
	assert.False(t, disabled.Enabled)
	assert.Equal(t, edited.Revision, disabled.Revision, "disable does not re-arm delivery")
	assert.Equal(t, edited.RowVersion+1, disabled.RowVersion)

	rec = testharness.Serve(dash, dashReq(t, http.MethodDelete,
		standingOrderActionPath(created.ID, "", edited.RowVersion), nil))
	assert.Equal(t, http.StatusConflict, rec.Code, "the disable invalidated the stale delete")

	rec = testharness.Serve(dash, dashReq(t, http.MethodPost,
		standingOrderActionPath(created.ID, "enable", disabled.RowVersion), nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	current, err := db.GetStandingOrder(created.ID)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.True(t, current.Enabled)
	assert.Equal(t, disabled.Revision+1, current.Revision, "manual enable re-arms delivery")
	assert.Equal(t, disabled.RowVersion+1, current.RowVersion)

	rec = testharness.Serve(dash, dashReq(t, http.MethodDelete,
		standingOrderActionPath(created.ID, "", current.RowVersion), nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	gone, err := db.GetStandingOrder(created.ID)
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestDashboardStandingOrders_ValidatesActionTriggerMatcher(t *testing.T) {
	newFlow(t)
	targetAgent, _, err := db.EnsureAgentForConv("conv-matcher-target", "test")
	require.NoError(t, err)
	dash := agentd.BuildDashboardHandlerForTest()

	body := standingOrderBody(targetAgent, 0)
	body["trigger_event"] = db.StandingTriggerUserPrompt
	body["sources"] = []string{}
	body["match_field"] = db.StandingMatchFieldPrompt
	body["match_regex"] = `(?i)\bdeploy\b`

	created, code := mutateStandingOrder(t, dash, http.MethodPost,
		"/api/standing-orders", body)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, created)
	assert.Equal(t, db.StandingTriggerUserPrompt, created.Trigger.Event)
	assert.Equal(t, db.StandingMatchFieldPrompt, created.Trigger.MatchField)
	assert.Equal(t, `(?i)\bdeploy\b`, created.Trigger.MatchRegex)
	assert.Contains(t, created.Trigger.Label, "prompt matches")
	assert.Equal(t, standingorders.StatusUnsupported,
		created.CapabilityByHarness["opencode"].Status)

	body["name"] = "invalid-lookahead"
	body["match_regex"] = `(?=deploy)`
	_, code = mutateStandingOrder(t, dash, http.MethodPost,
		"/api/standing-orders", body)
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestDashboardStandingOrders_EditPreservesAuthorAndRetirementState(t *testing.T) {
	f := newFlow(t)
	group := f.HaveGroup("authoring-team")
	ownerAgent, _, err := db.EnsureAgentForConv("conv-order-author", "test")
	require.NoError(t, err)
	id, err := db.InsertStandingOrder(&db.StandingOrder{
		Name: "agent-authored", OwnerAgent: ownerAgent,
		TargetKind: db.StandingTargetGroup, GroupID: group.ID,
		Summary:      "Keep the original attribution.",
		TriggerEvent: db.StandingTriggerSessionStart,
		Timing:       db.StandingTimingNextTurn, Cadence: db.StandingCadenceAlways,
		Enabled: true, OperatorAuthored: false,
	})
	require.NoError(t, err)
	beforeRetire, err := db.GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, beforeRetire)
	n, err := db.DisableGroupTargetStandingOrdersForRetire(group.ID)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	stored, err := db.GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, stored)

	staleBody := standingOrderBody("group:"+group.Name, beforeRetire.RowVersion)
	staleBody["name"] = "stale-retirement-overwrite"
	_, code := mutateStandingOrder(t, agentd.BuildDashboardHandlerForTest(),
		http.MethodPatch, fmt.Sprintf("/api/standing-orders/%d", id), staleBody)
	assert.Equal(t, http.StatusConflict, code,
		"an editor opened before automatic retirement cannot silently re-enable the order")

	body := standingOrderBody("group:"+group.Name, stored.RowVersion)
	body["name"] = "agent-authored-edited"
	body["enabled"] = false
	view, code := mutateStandingOrder(t, agentd.BuildDashboardHandlerForTest(),
		http.MethodPatch, fmt.Sprintf("/api/standing-orders/%d", id), body)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, view)
	assert.Equal(t, ownerAgent, view.OwnerAgent)
	assert.False(t, view.OperatorAuthored)
	assert.False(t, view.Enabled)
	assert.Equal(t, db.StandingDisabledReasonGroupRetired, view.DisabledReason)

	current, err := db.GetStandingOrder(id)
	require.NoError(t, err)
	body["row_version"] = current.RowVersion
	body["enabled"] = true
	view, code = mutateStandingOrder(t, agentd.BuildDashboardHandlerForTest(),
		http.MethodPatch, fmt.Sprintf("/api/standing-orders/%d", id), body)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, view.Enabled)
	assert.Empty(t, view.DisabledReason, "explicit re-enable acknowledges and clears the marker")
	assert.Equal(t, ownerAgent, view.OwnerAgent)
	assert.False(t, view.OperatorAuthored)
}
