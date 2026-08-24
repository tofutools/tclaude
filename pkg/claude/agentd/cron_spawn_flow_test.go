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
	"github.com/tofutools/tclaude/pkg/claude/harness"
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

// runCronNowWithDetail is runCronNow plus the recorded failure message, for
// tests that must prove WHICH gate decided rather than merely that something
// refused. run-now answers with the status alone; the reason is written to the
// job's run log.
func runCronNowWithDetail(t *testing.T, f *testharness.Flow, id int64) (string, string) {
	t.Helper()
	status := runCronNow(t, f, id)
	runs, err := db.ListAgentCronRunsForJob(id, 1)
	require.NoError(t, err)
	require.NotEmpty(t, runs, "a fire must leave a run row")
	return status, runs[0].ErrorMsg
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

func TestCronSpawnGlobalPermissionScopesAndForeignGroupAuthority(t *testing.T) {
	setup := func(t *testing.T, scope string) (*testharness.Flow, int64) {
		t.Helper()
		f := triggerFlow(t)
		g := f.HaveGroup("foreign")
		_, err := db.SetAgentGroupDefaultCwd(g.Name, t.TempDir())
		require.NoError(t, err)
		_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "scanner", Harness: harness.DefaultName})
		require.NoError(t, err)
		const owner = "cron-global-spawn-owner"
		f.HaveConvWithTitle(owner, "global spawner")
		f.HaveEnrolledAgent(owner)
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID: "sess-" + owner, TmuxSession: "tmux-" + owner, ConvID: owner,
			Cwd: f.World.HomeDir, Status: "running", Harness: harness.DefaultName,
			HarnessBuiltinMode: harness.ClaudeSandboxOff, ApprovalPolicy: "bypassPermissions",
		}))
		require.NoError(t, db.GrantAgentPermissionWithScope(owner, agentd.PermAgentSpawn, scope, "test"))
		require.NoError(t, db.GrantAgentPermissionWithScope(owner, agentd.PermGroupsMessagesSchedule,
			`{"group":["foreign"]}`, "test"))
		id, err := db.InsertAgentCronJob(&db.AgentCronJob{
			Name: "global", OwnerConv: owner, TargetKind: db.CronTargetGroup, GroupID: g.ID,
			IntervalSeconds: 3600, Enabled: true, ActionKind: db.CronActionSpawn,
			SpawnProfile: "scanner", SpawnInstructionTemplate: "scan",
			SpawnConcurrencyPolicy: db.CronConcurrencyForbid, SpawnMaxLiveWorkers: 1,
		})
		require.NoError(t, err)
		return f, id
	}

	t.Run("group and spawn profile scope authorizes a foreign group", func(t *testing.T) {
		f, id := setup(t, `{"group":["foreign"],"spawn_profile":["scanner"]}`)
		assert.Equal(t, "spawned", runCronNow(t, f, id))
	})

	t.Run("sandbox profile scope fails closed with nothing to inherit", func(t *testing.T) {
		f, id := setup(t, `{"group":["foreign"],"spawn_profile":["scanner"],"sandbox_profile":["locked"]}`)
		assert.Equal(t, "permission_denied", runCronNow(t, f, id))
	})

	// A managed worker never selects a sandbox profile, so the group's own
	// assignment is what the scope has to be read against — otherwise a
	// sandbox_profile-scoped grant could never fire a cron spawn at all.
	t.Run("sandbox profile scope matches the group assignment the worker inherits", func(t *testing.T) {
		f, id := setup(t, `{"group":["foreign"],"spawn_profile":["scanner"],"sandbox_profile":["locked"]}`)
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "locked"})
		require.NoError(t, err)
		_, err = db.SetAgentGroupSandboxProfile("foreign", "locked")
		require.NoError(t, err)
		assert.Equal(t, "spawned", runCronNow(t, f, id))
	})
}

// The managed-spawn counterpart of the launch-time binding check. A scoped
// owner's firing is refused when the spawn profile resolves a launch that would
// not enforce the inherited profile; an unscoped owner's identical firing is
// not, because it never traded on the profile.
func TestCronSpawnScopedSandboxProfileMustBindTheManagedWorker(t *testing.T) {
	setup := func(t *testing.T, scope string) (*testharness.Flow, int64) {
		t.Helper()
		f := triggerFlow(t)
		g := f.HaveGroup("workers")
		_, err := db.SetAgentGroupDefaultCwd(g.Name, t.TempDir())
		require.NoError(t, err)
		// The spawn profile itself resolves the profiles-omitting launch.
		_, err = db.CreateSpawnProfile(&db.SpawnProfile{
			Name: "unconfined", Harness: harness.CodexName,
			Sandbox: harness.SandboxDangerFull, Approval: "never",
		})
		require.NoError(t, err)
		_, err = db.CreateSandboxProfile(&db.SandboxProfile{Name: "locked"})
		require.NoError(t, err)
		_, err = db.SetAgentGroupSandboxProfile(g.Name, "locked")
		require.NoError(t, err)
		const owner = "cron-bind-owner"
		f.HaveConvWithTitle(owner, "cron owner")
		f.HaveEnrolledAgent(owner)
		// A member, so the group-restriction guardrail is not what decides
		// either subtest — the binding check below has to be the deciding gate.
		f.HaveMember(g.Name, owner)
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID: "sess-" + owner, TmuxSession: "tmux-" + owner, ConvID: owner,
			Cwd: f.World.HomeDir, Status: "running", Harness: harness.CodexName,
			HarnessBuiltinMode: harness.SandboxDangerFull, ApprovalPolicy: "never",
		}))
		if scope == "" {
			require.NoError(t, db.GrantAgentPermission(owner, agentd.PermGroupsMembersSpawn, "test"))
		} else {
			require.NoError(t, db.GrantAgentPermissionWithScope(owner,
				agentd.PermGroupsMembersSpawn, scope, "test"))
		}
		id, err := db.InsertAgentCronJob(&db.AgentCronJob{
			Name: "bind", OwnerConv: owner, TargetKind: db.CronTargetGroup, GroupID: g.ID,
			IntervalSeconds: 3600, Enabled: true, ActionKind: db.CronActionSpawn,
			SpawnProfile: "unconfined", SpawnInstructionTemplate: "scan",
			SpawnConcurrencyPolicy: db.CronConcurrencyForbid, SpawnMaxLiveWorkers: 1,
		})
		require.NoError(t, err)
		return f, id
	}

	t.Run("scoped owner is refused at fire time", func(t *testing.T) {
		f, id := setup(t, `{"sandbox_profile":["locked"]}`)
		status, detail := runCronNowWithDetail(t, f, id)
		assert.Equal(t, "permission_denied", status)
		assert.Contains(t, detail, "would not enforce it",
			"the binding check must be what refuses, not an unrelated guardrail")
	})

	t.Run("unscoped owner still fires", func(t *testing.T) {
		f, id := setup(t, "")
		assert.Equal(t, "spawned", runCronNow(t, f, id))
	})
}

func TestDashboardCronLogsPreserveWorkerIdentity(t *testing.T) {
	f := newFlow(t)
	group := f.HaveGroup("log-workers")
	jobID, err := db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "worker-log", TargetKind: db.CronTargetGroup, GroupID: group.ID,
		IntervalSeconds: 600, Body: "unused", Enabled: true,
	})
	require.NoError(t, err)
	_, err = db.InsertAgentCronRun(&db.AgentCronRun{
		JobID: jobID, FiredAt: time.Now().UTC(), Status: "spawned",
		WorkerID: 42, WorkerAgent: "agt_worker_identity",
	})
	require.NoError(t, err)

	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), dashReq(t, http.MethodGet,
		fmt.Sprintf("/api/cron/%d/logs", jobID), nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Runs []struct {
			WorkerID    int64  `json:"worker_id"`
			WorkerAgent string `json:"worker_agent"`
		} `json:"runs"`
	}
	testharness.DecodeJSON(t, rec, &out)
	require.Len(t, out.Runs, 1)
	assert.EqualValues(t, 42, out.Runs[0].WorkerID)
	assert.Equal(t, "agt_worker_identity", out.Runs[0].WorkerAgent)
}

func TestCronSpawnCreateWarnsAboutMissingGrantAndPatchEditsPayload(t *testing.T) {
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
		"spawn_concurrency_policy": db.CronConcurrencyReplace, "spawn_max_live_workers": 3,
		"spawn_worker_deadline_seconds": 60,
	})))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID       int64    `json:"id"`
		Warnings []string `json:"warnings"`
	}
	testharness.DecodeJSON(t, rec, &created)
	require.Len(t, created.Warnings, 1)
	assert.Contains(t, created.Warnings[0], agentd.PermGroupsMembersSpawn)
	require.NoError(t, db.GrantAgentPermissionWithScope(owner, agentd.PermAgentSpawn,
		`{"group":["alpha"],"spawn_profile":["scanner"]}`, "test"))
	allowed := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/cron", map[string]any{
		"name": "global-allowed", "target": "group:alpha", "owner": owner, "interval": "8h",
		"action_kind": "spawn", "spawn_profile": "scanner", "spawn_instruction_template": "scan",
	})))
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
	var allowedCreated struct {
		Warnings []string `json:"warnings"`
	}
	testharness.DecodeJSON(t, allowed, &allowedCreated)
	assert.Empty(t, allowedCreated.Warnings, "global agent.spawn authority satisfies cron preflight")

	patch := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPatch,
		fmt.Sprintf("/v1/cron/%d", created.ID), map[string]any{
			"spawn_instruction_template": "new scan", "spawn_roles": []string{},
		})))
	require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
	got, err := db.GetAgentCronJob(created.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "new scan", got.SpawnInstructionTemplate)
	assert.Empty(t, got.SpawnRoleRefs, "an explicit empty spawn_roles array clears the replacement set")
	assert.Equal(t, db.CronConcurrencyReplace, got.SpawnConcurrencyPolicy, "omitted fields remain unchanged")
	assert.Equal(t, 3, got.SpawnMaxLiveWorkers)
	assert.EqualValues(t, 60, got.SpawnWorkerDeadlineSeconds)
}
