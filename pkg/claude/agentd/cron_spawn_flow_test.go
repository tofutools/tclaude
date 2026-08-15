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
