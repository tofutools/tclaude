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

type groupStandingOrderRow struct {
	Order    standingorders.OrderView `json:"order"`
	Assigned bool                     `json:"assigned"`
	Primary  bool                     `json:"primary"`
	Global   bool                     `json:"global"`
}

type groupStandingOrdersPayload struct {
	Group  string                  `json:"group"`
	Orders []groupStandingOrderRow `json:"orders"`
}

func TestDashboardGroupStandingOrders_AssignReusableDefinitions(t *testing.T) {
	newFlow(t)
	primaryID, err := db.CreateAgentGroup("primary", "")
	require.NoError(t, err)
	additionalID, err := db.CreateAgentGroup("additional", "")
	require.NoError(t, err)
	dash := agentd.BuildDashboardHandlerForTest()

	primaryOrder := &db.StandingOrder{
		Name: "shared", TargetKind: db.StandingTargetGroup, GroupID: primaryID,
		Summary:      "Remember the shared instruction.",
		TriggerEvent: db.StandingTriggerSessionStart,
		Timing:       db.StandingTimingSameContinuation,
		Cadence:      db.StandingCadenceAlways, Enabled: true,
		OperatorAuthored: true,
	}
	orderID, err := db.InsertStandingOrder(primaryOrder)
	require.NoError(t, err)
	current, err := db.GetStandingOrder(orderID)
	require.NoError(t, err)

	global, code := mutateStandingOrder(t, dash, http.MethodPost,
		"/api/standing-orders", standingOrderBody("global", 0))
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, db.StandingTargetGlobal, global.Target.Kind)

	list := func(group string) groupStandingOrdersPayload {
		t.Helper()
		rec := testharness.Serve(dash, dashReq(t, http.MethodGet,
			"/api/groups/"+group+"/standing-orders", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var payload groupStandingOrdersPayload
		testharness.DecodeJSON(t, rec, &payload)
		return payload
	}
	find := func(payload groupStandingOrdersPayload, id int64) groupStandingOrderRow {
		t.Helper()
		for _, row := range payload.Orders {
			if row.Order.ID == id {
				return row
			}
		}
		t.Fatalf("order %d absent from group payload", id)
		return groupStandingOrderRow{}
	}

	additional := list("additional")
	assert.Equal(t, "additional", additional.Group)
	assert.False(t, find(additional, orderID).Assigned)
	assert.True(t, find(additional, global.ID).Global)

	assignPath := fmt.Sprintf(
		"/api/groups/additional/standing-orders/%d?row_version=%d",
		orderID, current.RowVersion)
	rec := testharness.Serve(dash, dashReq(t, http.MethodPost, assignPath, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var assigned groupStandingOrderRow
	testharness.DecodeJSON(t, rec, &assigned)
	assert.True(t, assigned.Assigned)
	assert.False(t, assigned.Primary)
	assert.Equal(t, current.Revision, assigned.Order.Revision,
		"an overlapping group path does not re-arm existing recipients")
	require.Len(t, assigned.Order.AdditionalGroups, 1)
	assert.Equal(t, additionalID, assigned.Order.AdditionalGroups[0].GroupID)

	rec = testharness.Serve(dash, dashReq(t, http.MethodDelete, assignPath, nil))
	assert.Equal(t, http.StatusConflict, rec.Code,
		"a stale Groups tab cannot undo a newer scope mutation")

	removePath := fmt.Sprintf(
		"/api/groups/additional/standing-orders/%d?row_version=%d",
		orderID, assigned.Order.RowVersion)
	rec = testharness.Serve(dash, dashReq(t, http.MethodDelete, removePath, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var removed groupStandingOrderRow
	testharness.DecodeJSON(t, rec, &removed)
	assert.False(t, removed.Assigned)
	assert.Empty(t, removed.Order.AdditionalGroups)

	primary := find(list("primary"), orderID)
	require.True(t, primary.Primary)
	primaryDelete := fmt.Sprintf(
		"/api/groups/primary/standing-orders/%d?row_version=%d",
		orderID, removed.Order.RowVersion)
	rec = testharness.Serve(dash, dashReq(t, http.MethodDelete, primaryDelete, nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"primary scope is changed through the Automations editor")

	globalAssign := fmt.Sprintf(
		"/api/groups/additional/standing-orders/%d?row_version=%d",
		global.ID, global.RowVersion)
	rec = testharness.Serve(dash, dashReq(t, http.MethodPost, globalAssign, nil))
	assert.Equal(t, http.StatusConflict, rec.Code,
		"a redundant group assignment is not stored for a global order")
}
