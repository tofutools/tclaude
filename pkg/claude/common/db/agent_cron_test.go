package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentCronJob_InsertGetList(t *testing.T) {
	setupTestDB(t)

	id, err := InsertAgentCronJob(&AgentCronJob{
		Name:            "po-pings",
		OwnerConv:       "po-conv",
		TargetConv:      "worker-conv",
		GroupID:         42,
		IntervalSeconds: 600,
		Subject:         "status check",
		Body:            "What's the latest?",
		Enabled:         true,
	})
	require.NoError(t, err, "InsertAgentCronJob")
	require.Greater(t, id, int64(0), "expected positive id")

	got, err := GetAgentCronJob(id)
	require.NoError(t, err, "GetAgentCronJob")
	require.NotNil(t, got, "got nil row")
	assert.Equal(t, "po-pings", got.Name, "round-trip mismatch: %+v", got)
	assert.Equal(t, "po-conv", got.OwnerConv, "round-trip mismatch: %+v", got)
	assert.Equal(t, "worker-conv", got.TargetConv, "round-trip mismatch: %+v", got)
	assert.Equal(t, int64(600), got.IntervalSeconds, "interval")
	assert.True(t, got.Enabled, "expected enabled=true")
	assert.False(t, got.CreatedAt.IsZero(), "created_at should be stamped on insert")
	assert.True(t, got.LastRunAt.IsZero(), "last_run_at should be zero before any fire")

	all, err := ListAgentCronJobs()
	require.NoError(t, err, "ListAgentCronJobs")
	require.Len(t, all, 1, "expected 1 job")
}

func TestAgentCronJob_DueLogic(t *testing.T) {
	setupTestDB(t)

	// j1: never run, always due.
	j1, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	// j2: ran 30s ago with a 60s interval — NOT due.
	j2, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	// j3: ran 90s ago with a 60s interval — due.
	j3, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	// j4: disabled, even though it'd be due — must NOT show up.
	j4, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: false,
	})

	now := time.Now()
	require.NoError(t, UpdateAgentCronJobLastRun(j2, now.Add(-30*time.Second), "ok"), "stamp j2")
	require.NoError(t, UpdateAgentCronJobLastRun(j3, now.Add(-90*time.Second), "ok"), "stamp j3")
	require.NoError(t, UpdateAgentCronJobLastRun(j4, now.Add(-90*time.Second), "ok"), "stamp j4")

	due, err := ListDueAgentCronJobs(now)
	require.NoError(t, err, "ListDueAgentCronJobs")
	dueIDs := map[int64]bool{}
	for _, j := range due {
		dueIDs[j.ID] = true
	}
	assert.True(t, dueIDs[j1], "j1 (never run) should be due")
	assert.False(t, dueIDs[j2], "j2 (30s ago, 60s interval) should NOT be due")
	assert.True(t, dueIDs[j3], "j3 (90s ago, 60s interval) should be due")
	assert.False(t, dueIDs[j4], "j4 (disabled) should never be due")
}

func TestAgentCronRun_InsertListCascade(t *testing.T) {
	setupTestDB(t)

	id, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	// Three runs at distinct timestamps.
	t0 := time.Now()
	for i, dt := range []time.Duration{-2 * time.Hour, -1 * time.Hour, 0} {
		_, err := InsertAgentCronRun(&AgentCronRun{
			JobID:   id,
			FiredAt: t0.Add(dt),
			Status:  "ok",
		})
		require.NoError(t, err, "insert run %d", i)
	}

	// Newest first.
	runs, err := ListAgentCronRunsForJob(id, 0)
	require.NoError(t, err, "list")
	require.Len(t, runs, 3, "expected 3 runs")
	assert.True(t, runs[0].FiredAt.After(runs[1].FiredAt) && runs[1].FiredAt.After(runs[2].FiredAt), "runs not sorted newest-first: %+v", runs)

	// Limit truncates from the head (newest).
	limited, _ := ListAgentCronRunsForJob(id, 2)
	assert.Len(t, limited, 2, "limit=2")

	// Cascade on job delete.
	require.NoError(t, DeleteAgentCronJob(id), "delete job")
	after, _ := ListAgentCronRunsForJob(id, 0)
	assert.Len(t, after, 0, "expected runs cascaded with job delete")
}

// TestAgentCronRun_SameSecondOrdering locks in newest-first ordering when
// several runs fire within the same whole second. fired_at is stored as a
// second-precision timestamp string, so same-second runs serialise
// identically — under ORDER BY fired_at their relative order is unspecified,
// and with LIMIT the genuinely-newest run can be dropped from a "last N runs"
// view. Ordering by id (autoincrement = insertion order) is deterministic and
// correct regardless of how the timestamps collapse. Same flake class as the
// inbox/outbox fix in #411.
func TestAgentCronRun_SameSecondOrdering(t *testing.T) {
	setupTestDB(t)

	jobID, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	// Five runs, all stamped on the exact same whole second → identical
	// fired_at strings. Insertion order (= id order) is the only thing that
	// distinguishes them.
	whole := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	var ids []int64
	for range 5 {
		runID, err := InsertAgentCronRun(&AgentCronRun{
			JobID: jobID, FiredAt: whole, Status: "ok",
		})
		require.NoError(t, err)
		ids = append(ids, runID)
	}

	// Full list: strictly descending by id (newest insertion first).
	runs, err := ListAgentCronRunsForJob(jobID, 0)
	require.NoError(t, err)
	require.Len(t, runs, 5)
	for i := range runs {
		assert.Equal(t, ids[len(ids)-1-i], runs[i].ID, "run %d out of newest-first order", i)
	}

	// Under LIMIT the newest runs must survive, not arbitrary same-second ties.
	limited, err := ListAgentCronRunsForJob(jobID, 2)
	require.NoError(t, err)
	require.Len(t, limited, 2)
	assert.Equal(t, ids[4], limited[0].ID, "newest run must be kept under LIMIT")
	assert.Equal(t, ids[3], limited[1].ID, "second-newest run must be kept under LIMIT")
}

func TestAgentCronJob_UpdateFields_Partial(t *testing.T) {
	setupTestDB(t)

	id, _ := InsertAgentCronJob(&AgentCronJob{
		Name:            "before",
		OwnerConv:       "owner",
		TargetConv:      "target",
		GroupID:         7,
		IntervalSeconds: 300,
		Subject:         "subj-before",
		Body:            "body-before",
		Enabled:         true,
	})
	// Stamp a non-zero last_run_at so we can prove UpdateAgentCronJobFields
	// leaves it alone.
	prevRun := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, UpdateAgentCronJobLastRun(id, prevRun, "ok"), "stamp")

	// Touch only name + enabled. All other fields should be unchanged.
	newName := "after"
	enabled := false
	n, err := UpdateAgentCronJobFields(id, UpdateCronPatch{Name: &newName, Enabled: &enabled})
	require.NoError(t, err, "UpdateAgentCronJobFields")
	require.Equal(t, 1, n, "rows affected")
	got, _ := GetAgentCronJob(id)
	assert.Equal(t, "after", got.Name, "name")
	assert.False(t, got.Enabled, "expected enabled=false after patch")
	// Untouched.
	assert.Equal(t, "owner", got.OwnerConv, "untouched fields changed: %+v", got)
	assert.Equal(t, "target", got.TargetConv, "untouched fields changed: %+v", got)
	assert.Equal(t, int64(7), got.GroupID, "untouched fields changed: %+v", got)
	assert.Equal(t, int64(300), got.IntervalSeconds, "untouched fields changed: %+v", got)
	assert.Equal(t, "subj-before", got.Subject, "untouched fields changed: %+v", got)
	assert.Equal(t, "body-before", got.Body, "untouched fields changed: %+v", got)
	// last_run_at preserved.
	assert.True(t, got.LastRunAt.Equal(prevRun), "last_run_at must not be touched; got %v, want %v", got.LastRunAt, prevRun)
	assert.Equal(t, "ok", got.LastRunStatus, "last_run_status must not be touched")
}

func TestAgentCronJob_UpdateFields_IntervalLeavesLastRunAlone(t *testing.T) {
	setupTestDB(t)

	id, _ := InsertAgentCronJob(&AgentCronJob{
		Name:            "n",
		OwnerConv:       "a",
		TargetConv:      "b",
		IntervalSeconds: 60,
		Body:            "x",
		Enabled:         true,
	})
	stamped := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Second)
	require.NoError(t, UpdateAgentCronJobLastRun(id, stamped, "ok"), "stamp")

	newInterval := int64(3600)
	_, err := UpdateAgentCronJobFields(id, UpdateCronPatch{IntervalSeconds: &newInterval})
	require.NoError(t, err, "UpdateAgentCronJobFields")
	got, _ := GetAgentCronJob(id)
	assert.Equal(t, int64(3600), got.IntervalSeconds, "interval")
	assert.True(t, got.LastRunAt.Equal(stamped), "last_run_at changed after interval patch: got %v, want %v", got.LastRunAt, stamped)
}

func TestAgentCronJob_UpdateFields_EmptyPatchNoop(t *testing.T) {
	setupTestDB(t)
	id, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})
	n, err := UpdateAgentCronJobFields(id, UpdateCronPatch{})
	require.NoError(t, err, "UpdateAgentCronJobFields")
	assert.Equal(t, 0, n, "empty patch should affect 0 rows")
}

func TestAgentCronJob_UpdateFields_NotFound(t *testing.T) {
	setupTestDB(t)
	newName := "x"
	n, err := UpdateAgentCronJobFields(9999, UpdateCronPatch{Name: &newName})
	require.NoError(t, err, "UpdateAgentCronJobFields")
	assert.Equal(t, 0, n, "missing id should affect 0 rows")
}

func TestAgentCronJob_DeleteAndEnable(t *testing.T) {
	setupTestDB(t)

	id, _ := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		IntervalSeconds: 60, Body: "x", Enabled: true,
	})

	require.NoError(t, SetAgentCronJobEnabled(id, false), "disable")
	got, _ := GetAgentCronJob(id)
	assert.False(t, got.Enabled, "expected enabled=false after disable")

	require.NoError(t, DeleteAgentCronJob(id), "delete")
	got, _ = GetAgentCronJob(id)
	assert.Nil(t, got, "expected nil after delete")
	// Idempotent on re-delete.
	assert.NoError(t, DeleteAgentCronJob(id), "re-delete should be no-op")
}
