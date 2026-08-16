package agentd_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// jobsPage decodes the /api/jobs envelope — the /api/retired family's shared
// shape with the unified {kind, export?, cron?} rows.
type jobsPage struct {
	Rows []struct {
		Kind   string `json:"kind"`
		Export *struct {
			ID     int64  `json:"id"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"export"`
		Cron *struct {
			ID                         int64    `json:"id"`
			Name                       string   `json:"name"`
			TargetRole                 string   `json:"target_role"`
			ActionKind                 string   `json:"action_kind"`
			SpawnProfile               string   `json:"spawn_profile"`
			SpawnRoles                 []string `json:"spawn_roles"`
			SpawnNameTemplate          string   `json:"spawn_name_template"`
			SpawnInstructionTemplate   string   `json:"spawn_instruction_template"`
			SpawnConcurrencyPolicy     string   `json:"spawn_concurrency_policy"`
			SpawnMaxLiveWorkers        int      `json:"spawn_max_live_workers"`
			SpawnWorkerDeadlineSeconds int64    `json:"spawn_worker_deadline_seconds"`
		} `json:"cron"`
		Order *struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Summary string `json:"summary"`
			Target  struct {
				GroupName string `json:"group_name"`
			} `json:"target"`
			Trigger struct {
				Label string `json:"label"`
			} `json:"trigger"`
			LastEvaluation *struct {
				Outcome string `json:"outcome"`
				Problem bool   `json:"problem"`
			} `json:"last_evaluation"`
			Capability *struct {
				Status string `json:"status"`
			} `json:"capability"`
		} `json:"order"`
	} `json:"rows"`
	Offset          int `json:"offset"`
	Limit           int `json:"limit"`
	Total           int `json:"total"`
	TotalUnfiltered int `json:"total_unfiltered"`
}

func getJobs(t *testing.T, dash http.Handler, query string) jobsPage {
	t.Helper()
	rec := testharness.Serve(dash, dashReq(t, http.MethodGet, "/api/jobs"+query, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page jobsPage
	testharness.DecodeJSON(t, rec, &page)
	return page
}

// TestDashboardJobs_UnifiedListing exercises GET /api/jobs — the Jobs tab's
// unified export + cron listing: kind discrimination, the server-side q
// filter (it searches the WHOLE set, not a page), offset/limit windowing and
// stale-offset clamping (the /api/retired contract).
func TestDashboardJobs_UnifiedListing(t *testing.T) {
	f := newFlow(t)
	group := f.HaveGroup("gamma-team")
	const orderTarget = "jjj00000-0000-4000-8000-000000000003"
	f.HaveMemberWithRole(group.Name, orderTarget, "reviewer")
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "standing-order-target", ConvID: orderTarget, Harness: "codex",
	}))
	dash := agentd.BuildDashboardHandlerForTest()

	_, err := db.InsertExportJob(&db.ExportJob{
		ConvID: "jjj00000-0000-4000-8000-000000000001", Title: "alpha report",
		Status: db.ExportStatusCloning,
	})
	require.NoError(t, err)
	_, err = db.InsertExportJob(&db.ExportJob{
		ConvID: "jjj00000-0000-4000-8000-000000000002", Title: "beta summary",
		Status: db.ExportStatusRunning,
	})
	require.NoError(t, err)
	_, err = db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "gamma-ping", TargetKind: db.CronTargetGroup,
		GroupID: group.ID, TargetRole: "reviewer",
		IntervalSeconds: 300, Subject: "ping", Body: "status?", Enabled: true,
		ActionKind: db.CronActionSpawn, SpawnProfile: "scanner", SpawnRoleRefs: []string{"reviewer"},
		SpawnNameTemplate: "worker", SpawnInstructionTemplate: "scan",
		SpawnConcurrencyPolicy: db.CronConcurrencyReplace, SpawnMaxLiveWorkers: 3,
		SpawnWorkerDeadlineSeconds: 120,
	})
	require.NoError(t, err)
	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:             "pr-early",
		TargetKind:       db.StandingTargetGroup,
		GroupID:          group.ID,
		TargetRole:       "reviewer",
		Summary:          "Push the pull request early.",
		TriggerEvent:     db.StandingTriggerSessionStart,
		TriggerSources:   []string{db.StandingSourceCompact},
		Timing:           db.StandingTimingSameContinuation,
		Cadence:          db.StandingCadenceAlways,
		Enabled:          true,
		OperatorAuthored: true,
	})
	require.NoError(t, err)
	additionalGroupID, err := db.CreateAgentGroup("delta-team", "")
	require.NoError(t, err)
	order, err := db.GetStandingOrder(orderID)
	require.NoError(t, err)
	_, err = db.SetStandingOrderGroupScope(
		orderID, additionalGroupID, order.RowVersion, true)
	require.NoError(t, err)
	_, err = db.RecordStandingDelivery(&db.StandingDelivery{
		OrderID:       orderID,
		OrderRevision: 1,
		TargetConv:    "jjj00000-0000-4000-8000-000000000002",
		Outcome:       db.StandingOutcomeDelivered,
		Transport:     db.StandingTransportHookContext,
		Harness:       "codex",
		Detail:        "SessionStart(source=compact) → hook-context",
	})
	require.NoError(t, err)

	// Unfiltered: every job kind in one list.
	all := getJobs(t, dash, "")
	assert.Equal(t, 4, all.Total)
	assert.Equal(t, 4, all.TotalUnfiltered)
	for _, row := range all.Rows {
		if row.Cron == nil {
			continue
		}
		assert.Equal(t, db.CronActionSpawn, row.Cron.ActionKind)
		assert.Equal(t, "scanner", row.Cron.SpawnProfile)
		assert.Equal(t, []string{"reviewer"}, row.Cron.SpawnRoles)
		assert.Equal(t, "worker", row.Cron.SpawnNameTemplate)
		assert.Equal(t, "scan", row.Cron.SpawnInstructionTemplate)
		assert.Equal(t, db.CronConcurrencyReplace, row.Cron.SpawnConcurrencyPolicy)
		assert.Equal(t, 3, row.Cron.SpawnMaxLiveWorkers)
		assert.EqualValues(t, 120, row.Cron.SpawnWorkerDeadlineSeconds)
	}
	require.Len(t, all.Rows, 4)
	kinds := map[string]int{}
	for _, r := range all.Rows {
		kinds[r.Kind]++
		switch r.Kind {
		case "export":
			require.NotNil(t, r.Export, "an export row must carry its export payload")
			assert.Nil(t, r.Cron)
		case "cron":
			require.NotNil(t, r.Cron, "a cron row must carry its cron payload")
			assert.Nil(t, r.Export)
			assert.Equal(t, "gamma-ping", r.Cron.Name)
			assert.Equal(t, "reviewer", r.Cron.TargetRole, "group role is available for edit prefill")
		case "standing-order":
			require.NotNil(t, r.Order, "a standing-order row must carry its order payload")
			assert.Nil(t, r.Export)
			assert.Nil(t, r.Cron)
			assert.Equal(t, "pr-early", r.Order.Name)
			assert.Equal(t, "gamma-team", r.Order.Target.GroupName)
			assert.Equal(t, "session.start (compact)", r.Order.Trigger.Label)
			require.NotNil(t, r.Order.LastEvaluation)
			assert.Equal(t, db.StandingOutcomeDelivered, r.Order.LastEvaluation.Outcome)
			assert.False(t, r.Order.LastEvaluation.Problem)
			require.NotNil(t, r.Order.Capability)
			assert.Equal(t, "supported", r.Order.Capability.Status)
		}
	}
	assert.Equal(t, map[string]int{"export": 2, "cron": 1, "standing-order": 1}, kinds)

	// The q filter is server-side and spans every kind's text fields.
	cronOnly := getJobs(t, dash, "?q=gamma-ping")
	assert.Equal(t, 1, cronOnly.Total)
	assert.Equal(t, 4, cronOnly.TotalUnfiltered, "total_unfiltered ignores q")
	require.Len(t, cronOnly.Rows, 1)
	assert.Equal(t, "cron", cronOnly.Rows[0].Kind)
	for _, query := range []string{"spawn", "scanner", "replace", "scan"} {
		spawnOnly := getJobs(t, dash, "?q="+query)
		require.Len(t, spawnOnly.Rows, 1, "spawn field %q participates in search", query)
		assert.Equal(t, "cron", spawnOnly.Rows[0].Kind)
	}

	exportOnly := getJobs(t, dash, "?q=alpha")
	require.Len(t, exportOnly.Rows, 1)
	require.Equal(t, "export", exportOnly.Rows[0].Kind)
	assert.Equal(t, "alpha report", exportOnly.Rows[0].Export.Title)

	additionalGroupOnly := getJobs(t, dash, "?q=delta-team")
	require.Len(t, additionalGroupOnly.Rows, 1)
	require.Equal(t, "standing-order", additionalGroupOnly.Rows[0].Kind)
	additionalGroupIDOnly := getJobs(t, dash,
		"?q=%23"+strconv.FormatInt(additionalGroupID, 10))
	require.Len(t, additionalGroupIDOnly.Rows, 1)
	require.Equal(t, "standing-order", additionalGroupIDOnly.Rows[0].Kind)

	// A kind name matches as filter text too — "export" finds both exports.
	byKind := getJobs(t, dash, "?q=export")
	assert.Equal(t, 2, byKind.Total)

	// Kind is a separate server-side view filter, applied before q and paging.
	// total_unfiltered is therefore the size of the selected view, not all jobs.
	schedules := getJobs(t, dash, "?kind=cron")
	assert.Equal(t, 1, schedules.Total)
	assert.Equal(t, 1, schedules.TotalUnfiltered)
	require.Len(t, schedules.Rows, 1)
	assert.Equal(t, "cron", schedules.Rows[0].Kind)

	standingOrders := getJobs(t, dash, "?kind=standing-order")
	assert.Equal(t, 1, standingOrders.Total)
	assert.Equal(t, 1, standingOrders.TotalUnfiltered)
	require.Len(t, standingOrders.Rows, 1)
	assert.Equal(t, "pr-early", standingOrders.Rows[0].Order.Name)

	explained := getJobs(t, dash, "?kind=standing-order&q=hook-context")
	require.Len(t, explained.Rows, 1, "latest evaluation fields participate in search")

	unknown := testharness.Serve(dash, dashReq(t, http.MethodGet, "/api/jobs?kind=surprise", nil))
	assert.Equal(t, http.StatusBadRequest, unknown.Code)

	// Windowing: limit bounds the page, total still counts everything.
	page1 := getJobs(t, dash, "?offset=0&limit=2")
	assert.Len(t, page1.Rows, 2)
	assert.Equal(t, 4, page1.Total)
	assert.Equal(t, 0, page1.Offset)

	// A stale offset past the end is clamped back to the last page.
	clamped := getJobs(t, dash, "?offset=99&limit=2")
	assert.Equal(t, 2, clamped.Offset, "stale offset clamps to the last page start")
	assert.Len(t, clamped.Rows, 2)
}

func TestDashboardJobs_SearchHandlesUnresolvedStandingOrderCapability(t *testing.T) {
	newFlow(t)
	targetAgent, _, err := db.EnsureAgentForConv("conv-with-no-session", "test")
	require.NoError(t, err)
	_, err = db.InsertStandingOrder(&db.StandingOrder{
		Name:             "unresolved-target",
		TargetKind:       db.StandingTargetConv,
		TargetAgent:      targetAgent,
		Summary:          "Still searchable while its target is offline.",
		TriggerEvent:     db.StandingTriggerSessionStart,
		Timing:           db.StandingTimingNextTurn,
		Cadence:          db.StandingCadenceAlways,
		Enabled:          true,
		OperatorAuthored: true,
	})
	require.NoError(t, err)

	page := getJobs(t, agentd.BuildDashboardHandlerForTest(),
		"?kind=standing-order&q=unresolved-target")
	require.Len(t, page.Rows, 1)
	require.NotNil(t, page.Rows[0].Order)
	assert.Nil(t, page.Rows[0].Order.Capability,
		"unknown target capability stays unknown rather than panicking or fabricating support")
}
