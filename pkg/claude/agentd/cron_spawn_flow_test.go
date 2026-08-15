package agentd_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func cronSpawnFixture(t *testing.T, policy string, maxLive int) (*testharness.Flow, *db.AgentCronJob) {
	t.Helper()
	f := triggerFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "scanner", Harness: "claude", Model: "sonnet"})
	require.NoError(t, err)
	job := createCronAsHuman(t, f, map[string]any{
		"name": "flaky-scanner", "target": "group:alpha", "interval": "8h",
		"action_kind": "spawn", "spawn_profile": "scanner",
		"spawn_instruction_template": "Check Linear for flaky-test tickets at {{fire_time}}; fix one unless a PR already exists.",
		"spawn_name_template":        "flaky-scanner-{{fire_time}}",
		"spawn_concurrency_policy":   policy, "spawn_max_live_workers": maxLive,
	})
	return f, job
}

func runCronNow(t *testing.T, f *testharness.Flow, id int64) string {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, fmt.Sprintf("/v1/cron/%d/run-now", id), nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Status string `json:"status"`
	}
	testharness.DecodeJSON(t, rec, &out)
	return out.Status
}

func TestCronSpawnConcurrencyPolicies(t *testing.T) {
	t.Run("Forbid skips while prior worker is live", func(t *testing.T) {
		f, job := cronSpawnFixture(t, db.CronConcurrencyForbid, 1)
		assert.Equal(t, "spawned", runCronNow(t, f, job.ID))
		assert.Equal(t, "skipped_concurrent", runCronNow(t, f, job.ID))
		workers, err := db.ListActiveCronWorkers(job.ID)
		require.NoError(t, err)
		assert.Len(t, workers, 1)
	})

	t.Run("Replace stops and replaces prior worker", func(t *testing.T) {
		f, job := cronSpawnFixture(t, db.CronConcurrencyReplace, 1)
		assert.Equal(t, "spawned", runCronNow(t, f, job.ID))
		first, err := db.ListActiveCronWorkers(job.ID)
		require.NoError(t, err)
		require.Len(t, first, 1)
		assert.Equal(t, "replace_stopped", runCronNow(t, f, job.ID))
		second, err := db.ListActiveCronWorkers(job.ID)
		require.NoError(t, err)
		require.Len(t, second, 1)
		assert.NotEqual(t, first[0].AgentID, second[0].AgentID)
	})

	t.Run("Allow is bounded by max live workers", func(t *testing.T) {
		f, job := cronSpawnFixture(t, db.CronConcurrencyAllow, 2)
		assert.Equal(t, "spawned", runCronNow(t, f, job.ID))
		assert.Equal(t, "spawned", runCronNow(t, f, job.ID))
		assert.Equal(t, "skipped_concurrent", runCronNow(t, f, job.ID))
		workers, err := db.ListActiveCronWorkers(job.ID)
		require.NoError(t, err)
		assert.Len(t, workers, 2)
	})
}

func TestCronSpawnRestartWindowAdvancesCadenceBeforeDispatch(t *testing.T) {
	for _, policy := range []string{db.CronConcurrencyForbid, db.CronConcurrencyReplace, db.CronConcurrencyAllow} {
		t.Run(policy, func(t *testing.T) {
			_, job := cronSpawnFixture(t, policy, 2)
			now := time.Now().UTC()
			d, err := db.Open()
			require.NoError(t, err)
			_, err = d.Exec(`UPDATE agent_cron_jobs SET created_at=? WHERE id=?`, now.Add(-9*time.Hour).UnixNano(), job.ID)
			require.NoError(t, err)
			const crash = "crash-after-cadence"
			restore := agentd.SetCronAfterSpawnCadenceForTest(func(int64) { panic(crash) })
			func() {
				defer func() { assert.Equal(t, crash, recover()) }()
				agentd.RunCronTickForTest(now)
			}()
			restore()
			workers, err := db.ListActiveCronWorkers(job.ID)
			require.NoError(t, err)
			assert.Empty(t, workers, "crash happened before reservation/dispatch")
			runs, err := db.ListAgentCronRunsForJob(job.ID, 10)
			require.NoError(t, err)
			require.Len(t, runs, 1)
			assert.Equal(t, "running", runs[0].Status)

			agentd.RunCronTickForTest(now.Add(time.Minute))
			runs, err = db.ListAgentCronRunsForJob(job.ID, 10)
			require.NoError(t, err)
			require.Len(t, runs, 1, "restart must not dispatch the same due tick again")
			assert.Equal(t, "interrupted", runs[0].Status)
		})
	}
}

func TestCronSpawnTickClosesStaleRunningRunAndOwnerlessReservation(t *testing.T) {
	_, job := cronSpawnFixture(t, db.CronConcurrencyForbid, 1)
	now := time.Now().UTC()
	runID, err := db.InsertAgentCronRun(&db.AgentCronRun{JobID: job.ID, FiredAt: now.Add(-2 * time.Minute), Status: "running"})
	require.NoError(t, err)
	_, err = db.InsertTriggerWorker(&db.TriggerWorker{CronJobID: job.ID, CronRunID: runID,
		ActionIndex: 0, AgentID: db.NewAgentID(), State: "reserved", CreatedAt: now.Add(-2 * time.Minute)})
	require.NoError(t, err)
	_, err = db.InsertTriggerWorker(&db.TriggerWorker{CronJobID: job.ID,
		ActionIndex: 0, AgentID: db.NewAgentID(), State: "reserved", CreatedAt: now.Add(-2 * time.Minute)})
	require.NoError(t, err)

	agentd.RunCronTickForTest(now)
	active, err := db.ListActiveCronWorkers(job.ID)
	require.NoError(t, err)
	assert.Empty(t, active, "stale and ownerless reservations cannot block Forbid forever")
	runs, err := db.ListAgentCronRunsForJob(job.ID, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "interrupted", runs[0].Status)
}

func TestCronSpawnReplaceFailsClosedWhenPriorWorkerCannotBeStopped(t *testing.T) {
	f, job := cronSpawnFixture(t, db.CronConcurrencyReplace, 1)
	_, err := db.InsertTriggerWorker(&db.TriggerWorker{
		CronJobID: job.ID, ActionIndex: 0, AgentID: db.NewAgentID(), State: "reserved", CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, "replace_stop_failed", runCronNow(t, f, job.ID))
	workers, err := db.ListActiveCronWorkers(job.ID)
	require.NoError(t, err)
	assert.Len(t, workers, 1, "failed stop must not launch a replacement")
}

func TestManagedSpawnLostPromotionRaceIsHonestForCronAndTrigger(t *testing.T) {
	losePromotion := func(workerID int64) {
		require.NoError(t, db.CompleteTriggerWorker(workerID, "deadline_exceeded", "test race", time.Now()))
	}

	t.Run("cron", func(t *testing.T) {
		f, job := cronSpawnFixture(t, db.CronConcurrencyForbid, 1)
		t.Cleanup(agentd.SetManagedWorkerBeforePromotionForTest(losePromotion))
		assert.Equal(t, "spawned_tracking_failed", runCronNow(t, f, job.ID))
		workers, err := db.ListActiveCronWorkers(job.ID)
		require.NoError(t, err)
		assert.Empty(t, workers)
	})

	t.Run("trigger", func(t *testing.T) {
		f := triggerFlow(t)
		f.HaveGroup("alpha")
		_, err := db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
		require.NoError(t, err)
		_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
		require.NoError(t, err)
		const author = "promotion-race-author"
		f.HaveConvWithTitle(author, "author")
		f.HaveMember("alpha", author)
		g, err := db.GetAgentGroupByName("alpha")
		require.NoError(t, err)
		ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "promotion-race", Enabled: true, OperatorAuthored: true,
			ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
			DraftFilter: db.TriggerDraftInclude, Actions: []db.TriggerAction{{Type: db.TriggerActionSpawn,
				Spawn: &db.TriggerSpawnAction{Profile: "reviewer", InstructionTemplate: "review", MaxLiveWorkers: 1}}}})
		require.NoError(t, err)
		authorAgent, err := db.AgentIDForConv(author)
		require.NoError(t, err)
		_, err = db.UpsertAgentPR(authorAgent, "https://github.com/o/r/pull/123", "ready", "open")
		require.NoError(t, err)
		t.Cleanup(agentd.SetManagedWorkerBeforePromotionForTest(losePromotion))
		agentd.RunTriggerTickForTest(time.Now().Add(time.Second))
		rows, err := db.ListTriggerFirings(ruleID, 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Len(t, rows[0].Actions, 1)
		assert.Equal(t, "spawned_tracking_failed", rows[0].Actions[0].Outcome)
		active, err := db.ListActiveTriggerWorkers()
		require.NoError(t, err)
		assert.Empty(t, active)
	})
}

func TestCronSpawnRestartEvidencePreventsDuplicateAndDenialIsNotRetried(t *testing.T) {
	t.Run("deadline terminal state reaches cron history", func(t *testing.T) {
		f, job := cronSpawnFixture(t, db.CronConcurrencyForbid, 1)
		// Patch the fixture row directly to keep the CLI-facing API in seconds
		// while making the flow deterministic and fast.
		d, err := db.Open()
		require.NoError(t, err)
		_, err = d.Exec(`UPDATE agent_cron_jobs SET spawn_worker_deadline_seconds=1 WHERE id=?`, job.ID)
		require.NoError(t, err)
		assert.Equal(t, "spawned", runCronNow(t, f, job.ID))
		agentd.RunTriggerTickForTest(time.Now().Add(2 * time.Second))
		workers, err := db.ListActiveCronWorkers(job.ID)
		require.NoError(t, err)
		assert.Empty(t, workers)
		runs, err := db.ListAgentCronRunsForJob(job.ID, 1)
		require.NoError(t, err)
		require.Len(t, runs, 1)
		assert.Equal(t, "deadline_exceeded", runs[0].Status)
	})

	t.Run("mid-flight worker survives scheduler restart evidence", func(t *testing.T) {
		f, job := cronSpawnFixture(t, db.CronConcurrencyForbid, 1)
		assert.Equal(t, "spawned", runCronNow(t, f, job.ID))
		// A later scheduler instance sees the durable active reservation. The
		// next firing is recorded as a concurrency skip, never a second spawn.
		assert.Equal(t, "skipped_concurrent", runCronNow(t, f, job.ID))
		workers, err := db.ListActiveCronWorkers(job.ID)
		require.NoError(t, err)
		assert.Len(t, workers, 1)
	})

	t.Run("fire-time permission denial advances cadence", func(t *testing.T) {
		f := triggerFlow(t)
		f.HaveGroup("alpha")
		_, err := db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
		require.NoError(t, err)
		_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "scanner", Harness: "claude"})
		require.NoError(t, err)
		const owner = "cron-denied-owner"
		f.HaveConvWithTitle(owner, "owner")
		f.HaveMember("alpha", owner)
		g, err := db.GetAgentGroupByName("alpha")
		require.NoError(t, err)
		id, err := db.InsertAgentCronJob(&db.AgentCronJob{
			Name: "denied", OwnerConv: owner, TargetKind: db.CronTargetGroup, GroupID: g.ID,
			IntervalSeconds: 3600, Enabled: true, ActionKind: db.CronActionSpawn,
			SpawnProfile: "scanner", SpawnInstructionTemplate: "scan", SpawnConcurrencyPolicy: db.CronConcurrencyForbid,
			SpawnMaxLiveWorkers: 1,
		})
		require.NoError(t, err)
		assert.Equal(t, "permission_denied", runCronNow(t, f, id))
		runs, err := db.ListAgentCronRunsForJob(id, 10)
		require.NoError(t, err)
		require.Len(t, runs, 1)
		before := time.Now()
		agentd.RunCronTickForTest(before.Add(time.Second))
		runs, err = db.ListAgentCronRunsForJob(id, 10)
		require.NoError(t, err)
		assert.Len(t, runs, 1, "the denied firing advanced last_run_at and was not retried")
	})
}

func TestCronSpawnFeatureGateLeavesMessageCronAvailable(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	spawn := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/cron", map[string]any{
		"target": "group:alpha", "interval": "1h", "action_kind": "spawn",
		"spawn_profile": "scanner", "spawn_instruction_template": "scan",
	})))
	assert.Equal(t, http.StatusNotFound, spawn.Code)
	message := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/cron", map[string]any{
		"target": "group:alpha", "interval": "1h", "body": "still works",
	})))
	assert.Equal(t, http.StatusOK, message.Code, message.Body.String())
}

func TestCronSpawnCreateWarnsAboutMissingGrantAndPatchRejectsUnsupportedEdits(t *testing.T) {
	f := triggerFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "scanner", Harness: "claude"})
	require.NoError(t, err)
	const owner = "cron-warning-owner"
	f.HaveConvWithTitle(owner, "owner")
	f.HaveMember("alpha", owner)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/cron", map[string]any{
		"name": "warning", "target": "group:alpha", "owner": owner, "interval": "8h",
		"action_kind": "spawn", "spawn_profile": "scanner", "spawn_instruction_template": "scan",
	})))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID       int64    `json:"id"`
		Warnings []string `json:"warnings"`
	}
	testharness.DecodeJSON(t, rec, &created)
	require.Len(t, created.Warnings, 1)
	assert.Contains(t, created.Warnings[0], agentd.PermGroupsMembersSpawn)

	for name, body := range map[string]map[string]any{
		"retarget":      {"target": "group:alpha"},
		"spawn payload": {"spawn_instruction_template": "new scan"},
		"action kind":   {"action_kind": "message"},
	} {
		t.Run(name, func(t *testing.T) {
			patch := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPatch,
				fmt.Sprintf("/v1/cron/%d", created.ID), body)))
			assert.Equal(t, http.StatusBadRequest, patch.Code, patch.Body.String())
			assert.Contains(t, patch.Body.String(), "recreate the cron job")
		})
	}
}
