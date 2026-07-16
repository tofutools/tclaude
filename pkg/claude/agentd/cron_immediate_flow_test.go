package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/cronexpr"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func createCronAsHuman(t *testing.T, f *testharness.Flow, body map[string]any) *db.AgentCronJob {
	t.Helper()
	// These tests exercise immediate/cadence semantics, not offline policy.
	// Preserve durable delivery without requiring a live pane in every case.
	if _, ok := body["queue_when_offline"]; !ok {
		body["queue_when_offline"] = true
	}
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/cron", body)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		ID             int64 `json:"id"`
		RunImmediately bool  `json:"run_immediately"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	job, err := db.GetAgentCronJob(out.ID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, job.RunImmediately, out.RunImmediately, "wire and persisted setting agree")
	return job
}

func patchCronAsHuman(t *testing.T, f *testharness.Flow, id int64, body map[string]any) {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPatch, "/v1/cron/"+strconv.FormatInt(id, 10), body)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestCronCreate_DefaultWaitsForFirstInterval(t *testing.T) {
	f := newFlow(t)
	const target = "ciwi-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "interval-waiter")

	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "interval": "30s", "body": "interval tick",
	})
	assert.False(t, job.RunImmediately)
	assert.True(t, job.LastRunAt.IsZero())
	assert.Zero(t, msgRowCount(t, target), "create must not deliver")

	agentd.RunCronTickForTest(job.CreatedAt.Add(29 * time.Second))
	assert.Zero(t, msgRowCount(t, target), "job is not due before one interval")
	agentd.RunCronTickForTest(job.CreatedAt.Add(31 * time.Second))
	assert.Equal(t, 1, msgRowCount(t, target), "first due interval delivers once")
	agentd.RunCronTickForTest(job.CreatedAt.Add(32 * time.Second))
	assert.Equal(t, 1, msgRowCount(t, target), "adjacent ticks do not replay the same due interval")
}

func TestCronCreate_DefaultWaitsForFirstExpressionMatch(t *testing.T) {
	f := newFlow(t)
	const target = "ciwe-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "expression-waiter")

	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "cron_expr": "* * * * *", "body": "expression tick",
	})
	assert.Zero(t, msgRowCount(t, target), "create must not deliver")
	agentd.RunCronTickForTest(job.CreatedAt.Add(2 * time.Minute))
	assert.Equal(t, 1, msgRowCount(t, target), "first expression match delivers once")
}

func TestCronCreate_RunImmediatelyOnceThenKeepsCadence(t *testing.T) {
	t.Run("interval", func(t *testing.T) {
		f := newFlow(t)
		const target = "ciii-aaaa-bbbb-cccc-000000000001"
		f.HaveConvWithTitle(target, "interval-immediate")
		job := createCronAsHuman(t, f, map[string]any{
			"target": target, "interval": "30s", "body": "interval immediate",
			"run_immediately": true,
		})
		assert.True(t, job.RunImmediately)
		assert.False(t, job.LastRunAt.IsZero())
		assert.Equal(t, 1, msgRowCount(t, target), "create delivers exactly once")
		agentd.RunCronTickForTest(job.LastRunAt.Add(20 * time.Second))
		assert.Equal(t, 1, msgRowCount(t, target), "no early cadence fire")
		agentd.RunCronTickForTest(job.LastRunAt.Add(31 * time.Second))
		assert.Equal(t, 2, msgRowCount(t, target), "normal interval follows immediate fire")
	})

	t.Run("expression", func(t *testing.T) {
		f := newFlow(t)
		const target = "ciie-aaaa-bbbb-cccc-000000000001"
		f.HaveConvWithTitle(target, "expression-immediate")
		job := createCronAsHuman(t, f, map[string]any{
			"target": target, "cron_expr": "@daily", "body": "expression immediate",
			"run_immediately": true,
		})
		assert.True(t, job.RunImmediately)
		assert.Equal(t, 1, msgRowCount(t, target), "create delivers exactly once")
		firstDue, err := cronexpr.Next(job.CronExpr, job.LastRunAt)
		require.NoError(t, err)
		require.False(t, firstDue.IsZero())
		agentd.RunCronTickForTest(firstDue.Add(-time.Nanosecond))
		assert.Equal(t, 1, msgRowCount(t, target), "expression does not fire before its first due time")
		agentd.RunCronTickForTest(firstDue)
		assert.Equal(t, 2, msgRowCount(t, target), "expression fires once at its first due time")
		agentd.RunCronTickForTest(firstDue.Add(time.Second))
		assert.Equal(t, 2, msgRowCount(t, target), "adjacent ticks do not replay the first due time")
	})
}

func TestCronPatch_RunImmediatelyIsEdgeTriggered(t *testing.T) {
	f := newFlow(t)
	const target = "cipe-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "patch-target")
	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "interval": "1h", "body": "patch immediate",
	})

	patchCronAsHuman(t, f, job.ID, map[string]any{"run_immediately": true})
	assert.Equal(t, 1, msgRowCount(t, target), "false→true fires once")
	patchCronAsHuman(t, f, job.ID, map[string]any{"run_immediately": true, "body": "edited"})
	assert.Equal(t, 1, msgRowCount(t, target), "true→true repeat save is inert")
	patchCronAsHuman(t, f, job.ID, map[string]any{"run_immediately": false})
	assert.Equal(t, 1, msgRowCount(t, target), "true→false is inert")
	patchCronAsHuman(t, f, job.ID, map[string]any{"run_immediately": true})
	assert.Equal(t, 2, msgRowCount(t, target), "a later false→true transition fires once")

	runs, err := db.ListAgentCronRunsForJob(job.ID, 0)
	require.NoError(t, err)
	assert.Len(t, runs, 2, "one history row per triggering edge")
}

func TestCronRunImmediatelyRejectsDisabledTransition(t *testing.T) {
	f := newFlow(t)
	const target = "cidis-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "disabled-target")

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/cron", map[string]any{
			"target": target, "interval": "1h", "body": "disabled create",
			"enabled": false, "run_immediately": true,
		})))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "interval": "1h", "body": "disabled patch", "enabled": false,
	})
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPatch, "/v1/cron/"+strconv.FormatInt(job.ID, 10),
		map[string]any{"run_immediately": true})))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	row, err := db.GetAgentCronJob(job.ID)
	require.NoError(t, err)
	assert.False(t, row.RunImmediately, "rejected transition is not persisted")
	assert.Zero(t, msgRowCount(t, target))
}

func TestCronPatch_ImmediateFireSuppressesCachedSchedulerCandidate(t *testing.T) {
	f := newFlow(t)
	const target = "cipr-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "race-target")
	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "interval": "30s", "body": "race tick",
	})
	d, err := db.Open()
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = d.Exec(`UPDATE agent_cron_jobs SET created_at = ? WHERE id = ?`,
		now.Add(-time.Hour).Format(time.RFC3339), job.ID)
	require.NoError(t, err)

	restore := agentd.SetCronAfterDueListForTest(func() {
		patchCronAsHuman(t, f, job.ID, map[string]any{"run_immediately": true})
	})
	t.Cleanup(restore)
	agentd.RunCronTickForTest(now)
	assert.Equal(t, 1, msgRowCount(t, target),
		"cached due candidate must re-check the cadence anchor after the immediate fire")
}

func TestCronLivenessSnapshotTimeoutReleasesAuthorityLock(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetTmuxCommandTimeoutForTest(time.Second))
	timeoutArmed, fireTimeout, restoreTimeout := agentd.ControlNextTmuxCommandTimeoutForTest()
	t.Cleanup(restoreTimeout)
	const target = "clst-target-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "snapshot-timeout-target")
	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "interval": "30s", "body": "bounded liveness",
		"queue_when_offline": false,
	})
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, db.UpdateAgentCronJobLastRun(job.ID, now.Add(-time.Hour), "ok"))

	// Model a tmux server that accepts the batch list command but never
	// answers. The scheduler holds cronAuthorityMu while revalidating/firing;
	// the bounded probe must release it so a mutation can complete.
	f.World.Tmux.HangNextCommand("list-sessions", 30*time.Second)
	tickDone := make(chan struct{})
	go func() {
		agentd.RunCronTickForTest(now)
		close(tickDone)
	}()
	select {
	case timeout := <-timeoutArmed:
		assert.Equal(t, time.Second, timeout)
	case <-time.After(10 * time.Second):
		t.Fatal("cron liveness snapshot did not start and arm its timeout")
	}

	patchDone := make(chan int, 1)
	go func() {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
			t, http.MethodPatch, "/v1/cron/"+strconv.FormatInt(job.ID, 10),
			map[string]any{"queue_when_offline": true})))
		patchDone <- rec.Code
	}()
	select {
	case <-patchDone:
		t.Fatal("cron PATCH unexpectedly bypassed the authority lock")
	case <-time.After(50 * time.Millisecond):
	}

	fireTimeout()
	select {
	case <-tickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler remained wedged after the tmux timeout")
	}
	select {
	case code := <-patchDone:
		assert.Equal(t, http.StatusOK, code)
	case <-time.After(5 * time.Second):
		t.Fatal("cron PATCH remained blocked after the tmux timeout")
	}
}

func TestCronRunNow_RevalidatesRetirementUnderAuthorityLock(t *testing.T) {
	f := newFlow(t)
	const owner = "cirn-owner-aaaa-bbbb-cccc-000000000001"
	const target = "cirn-target-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(owner, "run-now-owner")
	f.HaveConvWithTitle(target, "run-now-target")
	job := createCronAsHuman(t, f, map[string]any{
		"owner": owner, "target": target, "interval": "1h", "body": "must not fire",
	})

	restore := agentd.SetCronBeforeAuthorityLockForTest(func(operation string) {
		if operation != "run-now" {
			return
		}
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
			t, http.MethodPost, "/v1/agent/"+owner+"/retire?shutdown=0&delete_worktree=0", nil)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	t.Cleanup(restore)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/cron/"+strconv.FormatInt(job.ID, 10)+"/run-now", nil)))
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Zero(t, msgRowCount(t, target), "retired authority cannot deliver after commit")
}

func TestCronCreateImmediate_RetirementWinsBeforeAuthorityLock(t *testing.T) {
	f := newFlow(t)
	const owner = "cicr-owner-aaaa-bbbb-cccc-000000000001"
	const target = "cicr-target-aaaa-bbbb-cccc-000000000002"
	f.HaveConvWithTitle(owner, "create-owner")
	f.HaveConvWithTitle(target, "create-target")
	f.HaveEnrolledAgent(owner)

	restore := agentd.SetCronBeforeAuthorityLockForTest(func(operation string) {
		if operation != "create-immediate" {
			return
		}
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
			t, http.MethodPost, "/v1/agent/"+owner+"/retire?shutdown=0&delete_worktree=0", nil)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	t.Cleanup(restore)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPost, "/v1/cron", map[string]any{
			"owner": owner, "target": target, "interval": "1h", "body": "must not fire",
			"run_immediately": true,
		})))
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Zero(t, msgRowCount(t, target), "retirement before insert prevents immediate delivery")
}

func TestCronPatchImmediate_DisableWinsBeforeAuthorityLock(t *testing.T) {
	f := newFlow(t)
	const target = "cipd-target-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "patch-disable-target")
	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "interval": "1h", "body": "must stay paused",
	})

	restore := agentd.SetCronBeforeAuthorityLockForTest(func(operation string) {
		if operation != "patch" {
			return
		}
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
			t, http.MethodPost, "/v1/cron/"+strconv.FormatInt(job.ID, 10)+"/disable", nil)))
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	})
	t.Cleanup(restore)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(
		t, http.MethodPatch, "/v1/cron/"+strconv.FormatInt(job.ID, 10),
		map[string]any{"run_immediately": true})))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Zero(t, msgRowCount(t, target), "successful disable wins over the later immediate edit")
}

func TestDashboardCronRunNow_SuppressesCachedSchedulerCandidate(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const target = "cidr-target-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "dashboard-race-target")
	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "interval": "30s", "body": "dashboard race",
	})
	d, err := db.Open()
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = d.Exec(`UPDATE agent_cron_jobs SET created_at = ? WHERE id = ?`,
		now.Add(-time.Hour).Format(time.RFC3339), job.ID)
	require.NoError(t, err)
	mux := agentd.BuildDashboardHandlerForTest()

	restore := agentd.SetCronAfterDueListForTest(func() {
		rec := testharness.Serve(mux, testharness.JSONRequest(
			t, http.MethodPost, "/api/cron/"+strconv.FormatInt(job.ID, 10)+"/run-now", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
	t.Cleanup(restore)
	agentd.RunCronTickForTest(now)
	assert.Equal(t, 1, msgRowCount(t, target),
		"dashboard run-now and the cached scheduler candidate deliver only once")
}

func TestDashboardCronDelete_WaitsForAuthorizedScheduledDelivery(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	const target = "cidd-target-aaaa-bbbb-cccc-000000000001"
	f.HaveConvWithTitle(target, "dashboard-delete-target")
	job := createCronAsHuman(t, f, map[string]any{
		"target": target, "interval": "30s", "body": "deleted race",
	})
	d, err := db.Open()
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = d.Exec(`UPDATE agent_cron_jobs SET created_at = ? WHERE id = ?`,
		now.Add(-time.Hour).Format(time.RFC3339), job.ID)
	require.NoError(t, err)
	mux := agentd.BuildDashboardHandlerForTest()

	revalidated := make(chan struct{})
	allowFire := make(chan struct{})
	restoreRevalidation := agentd.SetCronAfterAuthorityRevalidationForTest(func(id int64) {
		if id != job.ID {
			return
		}
		close(revalidated)
		<-allowFire
	})
	t.Cleanup(restoreRevalidation)
	deleteAttempting := make(chan struct{})
	restoreBeforeLock := agentd.SetCronBeforeAuthorityLockForTest(func(operation string) {
		if operation == "delete" {
			close(deleteAttempting)
		}
	})
	t.Cleanup(restoreBeforeLock)
	t.Cleanup(func() {
		select {
		case <-allowFire:
		default:
			close(allowFire)
		}
	})

	tickDone := make(chan struct{})
	go func() {
		agentd.RunCronTickForTest(now)
		close(tickDone)
	}()
	select {
	case <-revalidated:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not reach authority revalidation")
	}

	deleteDone := make(chan int, 1)
	go func() {
		rec := testharness.Serve(mux, testharness.JSONRequest(
			t, http.MethodDelete, "/api/cron/"+strconv.FormatInt(job.ID, 10), nil))
		deleteDone <- rec.Code
	}()
	select {
	case <-deleteAttempting:
	case <-time.After(2 * time.Second):
		t.Fatal("delete did not reach the authority lock")
	}
	select {
	case code := <-deleteDone:
		t.Fatalf("delete returned %d before the authorized delivery completed", code)
	case <-time.After(50 * time.Millisecond):
	}

	close(allowFire)
	select {
	case <-tickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not complete delivery")
	}
	select {
	case code := <-deleteDone:
		require.Equal(t, http.StatusNoContent, code)
	case <-time.After(2 * time.Second):
		t.Fatal("delete did not complete after delivery released the authority lock")
	}
	assert.Equal(t, 1, msgRowCount(t, target),
		"delivery that won authority finishes before delete returns")
}
