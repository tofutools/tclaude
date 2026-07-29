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

func standingOrderBody(target string, revision int64) map[string]any {
	return map[string]any{
		"name": "boundary-reminder", "revision": revision, "target": target,
		"summary": "Re-read the durable instructions.", "trigger_event": "session.start",
		"sources": []string{"compact", "resume"}, "timing": "same-continuation",
		"cadence": "always", "enabled": true,
	}
}

func TestDashboardStandingOrders_CRUDLifecycleAndRevisionGuards(t *testing.T) {
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
	assert.Equal(t, targetAgent, created.Target.Agent)
	assert.True(t, created.OperatorAuthored)

	editBody := standingOrderBody(targetAgent, created.Revision)
	editBody["name"] = "boundary-reminder-edited"
	editBody["summary"] = "Updated durable instruction."
	editBody["timing"] = "next-turn"
	edited, code := mutateStandingOrder(t, dash, http.MethodPatch,
		fmt.Sprintf("/api/standing-orders/%d", created.ID), editBody)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, edited)
	assert.Equal(t, int64(2), edited.Revision)
	assert.Equal(t, "boundary-reminder-edited", edited.Name)
	assert.Equal(t, db.StandingTimingNextTurn, edited.Timing)

	_, code = mutateStandingOrder(t, dash, http.MethodPatch,
		fmt.Sprintf("/api/standing-orders/%d", created.ID), editBody)
	assert.Equal(t, http.StatusConflict, code, "a stale editor cannot overwrite a newer revision")

	rec := testharness.Serve(dash, dashReq(t, http.MethodPost,
		fmt.Sprintf("/api/standing-orders/%d/disable", created.ID), nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	disabled, err := db.GetStandingOrder(created.ID)
	require.NoError(t, err)
	require.NotNil(t, disabled)
	assert.False(t, disabled.Enabled)

	rec = testharness.Serve(dash, dashReq(t, http.MethodDelete,
		fmt.Sprintf("/api/standing-orders/%d?revision=%d", created.ID, edited.Revision), nil))
	assert.Equal(t, http.StatusConflict, rec.Code, "the disable invalidated the stale delete")

	rec = testharness.Serve(dash, dashReq(t, http.MethodPost,
		fmt.Sprintf("/api/standing-orders/%d/enable", created.ID), nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	current, err := db.GetStandingOrder(created.ID)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.True(t, current.Enabled)

	rec = testharness.Serve(dash, dashReq(t, http.MethodDelete,
		fmt.Sprintf("/api/standing-orders/%d?revision=%d", created.ID, current.Revision), nil))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	gone, err := db.GetStandingOrder(created.ID)
	require.NoError(t, err)
	assert.Nil(t, gone)
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
	n, err := db.DisableGroupTargetStandingOrdersForRetire(group.ID)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	stored, err := db.GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, stored)

	body := standingOrderBody("group:"+group.Name, stored.Revision)
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
	body["revision"] = current.Revision
	body["enabled"] = true
	view, code = mutateStandingOrder(t, agentd.BuildDashboardHandlerForTest(),
		http.MethodPatch, fmt.Sprintf("/api/standing-orders/%d", id), body)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, view.Enabled)
	assert.Empty(t, view.DisabledReason, "explicit re-enable acknowledges and clears the marker")
	assert.Equal(t, ownerAgent, view.OwnerAgent)
	assert.False(t, view.OperatorAuthored)
}
