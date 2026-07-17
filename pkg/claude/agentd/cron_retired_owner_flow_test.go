package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const cronRetiredOwnerWireMessage = "cron job owner is retired; the requested action was not applied"

func assertCronRetiredOwnerResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	testharness.DecodeJSON(t, rec, &body)
	assert.Equal(t, "not_runnable", body.Code)
	assert.Equal(t, cronRetiredOwnerWireMessage, body.Error)
}

func TestCronRetiredOwner_AllMutationSurfacesRejectWithoutSideEffects(t *testing.T) {
	f := newFlow(t)
	const owner = "crro-owner-aaaa-bbbb-cccc-000000000001"
	const target = "crro-target-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(owner, "retired-cron-owner")
	f.HaveConvWithTitle(target, "retired-cron-target")
	job := createCronAsHuman(t, f, map[string]any{
		"owner": owner, "target": target, "interval": "1h",
		"name": "retired-owned", "subject": "before", "body": "before-body",
		"queue_when_offline": true,
	})
	stamped := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	require.NoError(t, db.UpdateAgentCronJobLastRun(job.ID, stamped, "ok"))

	retire := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/agent/"+owner+"/retire?shutdown=0&delete_worktree=0", nil)))
	require.Equal(t, http.StatusOK, retire.Code, retire.Body.String())
	before, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	require.NotNil(t, before)
	require.False(t, before.Enabled)
	require.Equal(t, db.CronDisabledReasonAgentRetired, before.DisabledReason)

	jobPath := "/v1/cron/" + strconv.FormatInt(job.ID, 10)
	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{name: "enable endpoint", method: http.MethodPost, path: jobPath + "/enable"},
		{name: "disable endpoint", method: http.MethodPost, path: jobPath + "/disable"},
		{name: "body edit", method: http.MethodPatch, path: jobPath, body: map[string]any{"body": "after-body"}},
		{name: "schedule edit", method: http.MethodPatch, path: jobPath, body: map[string]any{"cron_expr": "0 9 * * *"}},
		{name: "explicit enable", method: http.MethodPatch, path: jobPath, body: map[string]any{"enabled": true}},
		{name: "explicit disable", method: http.MethodPatch, path: jobPath, body: map[string]any{"enabled": false}},
		{name: "immediate preference", method: http.MethodPatch, path: jobPath, body: map[string]any{"run_immediately": true, "enabled": true}},
		{name: "manual run", method: http.MethodPost, path: jobPath + "/run-now"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
				testharness.JSONRequest(t, tc.method, tc.path, tc.body)))
			assertCronRetiredOwnerResponse(t, rec)
			after, getErr := db.GetAgentCronJob(job.ID)
			require.NoError(t, getErr)
			assert.Equal(t, before, after, "denied request changed the stored job")
			runs, runsErr := db.ListAgentCronRunsForJob(job.ID, 0)
			require.NoError(t, runsErr)
			assert.Empty(t, runs, "denied request recorded an immediate/scheduled run")
			assert.Zero(t, msgRowCount(t, target), "denied request delivered a scheduler message")
		})
	}

	// The scheduler's independent owner revalidation also remains inert after
	// every denied mutation: no cadence stamp, run row, or notification appears.
	agentd.RunCronTickForTest(time.Now().Add(24 * time.Hour))
	afterTick, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, before, afterTick)
	runs, err := db.ListAgentCronRunsForJob(job.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, runs)
	assert.Zero(t, msgRowCount(t, target))
}

func TestDashboardCronRetiredOwner_PatchReturnsStableError(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const owner = "crrd-owner-aaaa-bbbb-cccc-000000000001"
	const target = "crrd-target-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(owner, "dashboard-retired-owner")
	f.HaveConvWithTitle(target, "dashboard-retired-target")
	job := createCronAsHuman(t, f, map[string]any{
		"owner": owner, "target": target, "interval": "1h", "body": "before",
	})
	_, err := db.RetireAgentAuthorizationByConv(owner, "human", "test")
	require.NoError(t, err)
	before, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)

	dashboard := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dashboard, testharness.JSONRequest(t, http.MethodPatch,
		"/api/cron/"+strconv.FormatInt(job.ID, 10), map[string]any{
			"body": "after", "interval": "30s", "enabled": true,
		}))
	assertCronRetiredOwnerResponse(t, rec)
	after, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Zero(t, msgRowCount(t, target))
}

func TestCronLiveOwner_UnchangedMutationRemainsSuccessfulNoop(t *testing.T) {
	f := newFlow(t)
	const owner = "crln-owner-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(owner, "live-cron-owner")
	job := createCronAsHuman(t, f, map[string]any{
		"owner": owner, "target": owner, "interval": "1h", "name": "unchanged",
		"subject": "same", "body": "same-body", "enabled": false,
	})
	before, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	path := "/v1/cron/" + strconv.FormatInt(job.ID, 10)

	patch := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPatch, path, map[string]any{
			"name": before.Name, "subject": before.Subject, "body": before.Body,
			"enabled": before.Enabled, "run_immediately": before.RunImmediately,
			"queue_when_offline": before.QueueWhenOffline,
		})))
	require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
	afterPatch, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, before, afterPatch)

	disable := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodPost, path+"/disable", nil)))
	require.Equal(t, http.StatusNoContent, disable.Code, disable.Body.String())
	afterDisable, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.Equal(t, before, afterDisable)
	assert.Zero(t, msgRowCount(t, owner))
}
