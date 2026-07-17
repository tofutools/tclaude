package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentCronJob_InsertGetList(t *testing.T) {
	setupTestDB(t)

	id, err := InsertAgentCronJob(&AgentCronJob{
		Name:             "po-pings",
		OwnerConv:        "po-conv",
		TargetConv:       "worker-conv",
		GroupID:          42,
		IntervalSeconds:  600,
		Subject:          "status check",
		Body:             "What's the latest?",
		Enabled:          true,
		RunImmediately:   true,
		QueueWhenOffline: true,
	})
	require.NoError(t, err, "InsertAgentCronJob")
	require.Greater(t, id, int64(0), "expected positive id")

	got, err := GetAgentCronJob(id)
	require.NoError(t, err, "GetAgentCronJob")
	require.NotNil(t, got, "got nil row")
	assert.Equal(t, "po-pings", got.Name, "round-trip mismatch: %+v", got)
	assert.Equal(t, "po-conv", got.OwnerConv, "round-trip mismatch: %+v", got)
	assert.Equal(t, "worker-conv", got.TargetConv, "round-trip mismatch: %+v", got)
	// PR3c: the job carries the stable owner/target agent_ids it is keyed on,
	// so the `cron ls` TARGET column can render the rotation-immune handle.
	ownerAgent, err := AgentIDForConv("po-conv")
	require.NoError(t, err, "AgentIDForConv(po-conv)")
	targetAgent, err := AgentIDForConv("worker-conv")
	require.NoError(t, err, "AgentIDForConv(worker-conv)")
	require.NotEmpty(t, ownerAgent, "owner conv should be minted as an actor")
	require.NotEmpty(t, targetAgent, "target conv should be minted as an actor")
	assert.Equal(t, ownerAgent, got.OwnerAgent, "job should carry the stable owner agent_id")
	assert.Equal(t, targetAgent, got.TargetAgent, "job should carry the stable target agent_id")
	assert.Equal(t, int64(600), got.IntervalSeconds, "interval")
	assert.True(t, got.Enabled, "expected enabled=true")
	assert.True(t, got.RunImmediately, "run_immediately round-trip")
	assert.True(t, got.QueueWhenOffline, "queue_when_offline round-trip")
	assert.False(t, got.CreatedAt.IsZero(), "created_at should be stamped on insert")
	assert.True(t, got.LastRunAt.IsZero(), "last_run_at should be zero before any fire")

	all, err := ListAgentCronJobs()
	require.NoError(t, err, "ListAgentCronJobs")
	require.Len(t, all, 1, "expected 1 job")
}

func TestAgentCronJob_DueLogic(t *testing.T) {
	setupTestDB(t)

	// j1: never run, anchored at creation — waits one full interval.
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
	assert.False(t, dueIDs[j1], "j1 (new) should wait for its first interval")
	assert.False(t, dueIDs[j2], "j2 (30s ago, 60s interval) should NOT be due")
	assert.True(t, dueIDs[j3], "j3 (90s ago, 60s interval) should be due")
	assert.False(t, dueIDs[j4], "j4 (disabled) should never be due")

	due, err = ListDueAgentCronJobs(now.Add(61 * time.Second))
	require.NoError(t, err)
	dueIDs = map[int64]bool{}
	for _, j := range due {
		dueIDs[j.ID] = true
	}
	assert.True(t, dueIDs[j1], "j1 should be due after its first interval")
}

func TestAgentCronJob_DueLogicExcludesRetiredOwner(t *testing.T) {
	setupTestDB(t)
	const conv = "cron-retired-owner"
	jobID, err := InsertAgentCronJob(&AgentCronJob{
		Name: "retired-owner", OwnerConv: conv, TargetConv: conv,
		IntervalSeconds: 60, Body: "must not fire", Enabled: true,
	})
	require.NoError(t, err)

	out, err := RetireAgentAuthorizationByConv(conv, "human", "test")
	require.NoError(t, err)
	require.True(t, out.Retired)
	require.Equal(t, int64(1), out.CronDisabled)

	// Every supported writer returns the same classifiable denial. The due query
	// is an independent defense against a stale/hand-edited row.
	require.ErrorIs(t, SetAgentCronJobEnabled(jobID, true), ErrAgentCronOwnerRetired)
	require.ErrorIs(t, SetAgentCronJobEnabled(jobID, false), ErrAgentCronOwnerRetired)
	enabled := true
	n, err := UpdateAgentCronJobFields(jobID, UpdateCronPatch{Enabled: &enabled})
	require.ErrorIs(t, err, ErrAgentCronOwnerRetired)
	assert.Zero(t, n, "denied patch reports no affected row")
	_, err = GetLiveOwnerAgentCronJob(jobID)
	require.ErrorIs(t, err, ErrAgentCronOwnerRetired)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE agent_cron_jobs SET enabled = 1 WHERE id = ?`, jobID)
	require.NoError(t, err)
	due, err := ListDueAgentCronJobs(time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, due)
}

func TestAgentCronJob_RetiredOwnerRejectsEveryFieldMutationAtomically(t *testing.T) {
	setupTestDB(t)
	const owner = "cron-retired-patch-owner"
	const target = "cron-retired-patch-target"
	jobID, err := InsertAgentCronJob(&AgentCronJob{
		Name: "before", OwnerConv: owner, TargetConv: target,
		IntervalSeconds: 60, Subject: "before-subject", Body: "before-body",
		Enabled: true, QueueWhenOffline: true,
	})
	require.NoError(t, err)
	require.NoError(t, UpdateAgentCronJobLastRun(jobID,
		time.Now().Add(-time.Hour).UTC().Truncate(time.Second), "ok"))
	_, err = RetireAgentAuthorizationByConv(owner, "human", "test")
	require.NoError(t, err)

	before, err := GetAgentCronJob(jobID)
	require.NoError(t, err)
	require.NotNil(t, before)
	name := "after"
	interval := int64(3600)
	cronExpr := "0 9 * * *"
	subject := "after-subject"
	body := "after-body"
	enabled := true
	runImmediately := true
	queueWhenOffline := false
	replacementOwner := "cron-denied-replacement-owner"
	replacementTarget := "cron-denied-replacement-target"
	for label, patch := range map[string]UpdateCronPatch{
		"name":                {Name: &name},
		"owner":               {OwnerConv: &replacementOwner},
		"target":              {TargetConv: &replacementTarget},
		"interval":            {IntervalSeconds: &interval},
		"cron expression":     {CronExpr: &cronExpr},
		"subject":             {Subject: &subject},
		"body":                {Body: &body},
		"enable":              {Enabled: &enabled},
		"run immediately":     {RunImmediately: &runImmediately},
		"queue while offline": {QueueWhenOffline: &queueWhenOffline},
	} {
		t.Run(label, func(t *testing.T) {
			n, updateErr := UpdateAgentCronJobFields(jobID, patch)
			require.ErrorIs(t, updateErr, ErrAgentCronOwnerRetired)
			assert.Zero(t, n)
			after, getErr := GetAgentCronJob(jobID)
			require.NoError(t, getErr)
			assert.Equal(t, before, after, "denied mutation changed the stored row")
		})
	}
	for _, conv := range []string{replacementOwner, replacementTarget} {
		agentID, lookupErr := AgentIDForConv(conv)
		require.NoError(t, lookupErr)
		assert.Empty(t, agentID, "denied patch enrolled %s outside the rolled-back transaction", conv)
	}
}

func TestAgentCronJob_RetiredOwnerDenialIsNotOrdinaryNoop(t *testing.T) {
	setupTestDB(t)
	const liveOwner = "cron-live-noop-owner"
	liveID, err := InsertAgentCronJob(&AgentCronJob{
		Name: "same", OwnerConv: liveOwner, TargetConv: liveOwner,
		IntervalSeconds: 60, Body: "same-body", Enabled: false,
	})
	require.NoError(t, err)
	before, err := GetAgentCronJob(liveID)
	require.NoError(t, err)

	// Exact unchanged writes from a live owner retain their historical success
	// semantics; classification is based on owner state, not RowsAffected.
	name := before.Name
	body := before.Body
	enabled := before.Enabled
	n, err := UpdateAgentCronJobFields(liveID, UpdateCronPatch{
		Name: &name, Body: &body, Enabled: &enabled,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.NoError(t, SetAgentCronJobEnabled(liveID, before.Enabled))
	after, err := GetAgentCronJob(liveID)
	require.NoError(t, err)
	assert.Equal(t, before, after)

	_, err = RetireAgentAuthorizationByConv(liveOwner, "human", "test")
	require.NoError(t, err)
	retiredBefore, err := GetAgentCronJob(liveID)
	require.NoError(t, err)
	n, err = UpdateAgentCronJobFields(liveID, UpdateCronPatch{Name: &name})
	require.ErrorIs(t, err, ErrAgentCronOwnerRetired)
	assert.Zero(t, n)
	retiredAfter, err := GetAgentCronJob(liveID)
	require.NoError(t, err)
	assert.Equal(t, retiredBefore, retiredAfter)
}

func TestAgentCronJob_RejectsRetiredOwnerOnInsertAndReassignment(t *testing.T) {
	setupTestDB(t)
	const retiredOwner = "cron-retired-new-owner"
	if _, _, err := EnsureAgentForConv(retiredOwner, "test"); err != nil {
		t.Fatal(err)
	}
	_, err := RetireAgentAuthorizationByConv(retiredOwner, "human", "test")
	require.NoError(t, err)

	_, err = InsertAgentCronJob(&AgentCronJob{
		OwnerConv: retiredOwner, TargetConv: "cron-insert-target",
		IntervalSeconds: 60, Body: "must not insert", Enabled: true,
	})
	require.ErrorIs(t, err, ErrAgentCronOwnerRetired)
	jobs, err := ListAgentCronJobs()
	require.NoError(t, err)
	assert.Empty(t, jobs)
	insertTargetAgent, err := AgentIDForConv("cron-insert-target")
	require.NoError(t, err)
	assert.Empty(t, insertTargetAgent, "denied insert enrolled its target")

	liveID, err := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "cron-live-reassign-owner", TargetConv: "cron-reassign-target",
		IntervalSeconds: 60, Body: "must stay live-owned", Enabled: false,
	})
	require.NoError(t, err)
	before, err := GetAgentCronJob(liveID)
	require.NoError(t, err)
	replacementOwner := retiredOwner
	n, err := UpdateAgentCronJobFields(liveID, UpdateCronPatch{OwnerConv: &replacementOwner})
	require.ErrorIs(t, err, ErrAgentCronOwnerRetired)
	assert.Zero(t, n)
	after, err := GetAgentCronJob(liveID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestRetireRestampsAlreadyPausedOwnedCronJob(t *testing.T) {
	setupTestDB(t)
	const conv = "cron-paused-owner"
	groupID, err := CreateAgentGroup("paused-cron-group", "")
	require.NoError(t, err)
	jobID, err := InsertAgentCronJob(&AgentCronJob{
		Name: "already-group-paused", OwnerConv: conv, TargetKind: CronTargetGroup,
		GroupID: groupID, IntervalSeconds: 60, Body: "must stay paused", Enabled: false,
	})
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE agent_cron_jobs SET disabled_reason = ? WHERE id = ?`,
		CronDisabledReasonGroupRetired, jobID)
	require.NoError(t, err)

	out, err := RetireAgentAuthorizationByConv(conv, "human", "test")
	require.NoError(t, err)
	assert.Equal(t, int64(1), out.CronDisabled, "marker restamp is a durable authority change")
	job, err := GetAgentCronJob(jobID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, CronDisabledReasonAgentRetired, job.DisabledReason)
	_, err = d.Exec(`UPDATE agent_cron_jobs SET disabled_reason = ? WHERE id = ?`,
		CronDisabledReasonGroupRetired, jobID)
	require.NoError(t, err)
	n, err := ReenableGroupRetiredCronJobs(groupID)
	require.NoError(t, err)
	assert.Zero(t, n, "group resume requires a live owner")
	_, err = d.Exec(`UPDATE agent_cron_jobs SET disabled_reason = ? WHERE id = ?`,
		CronDisabledReasonAgentRetired, jobID)
	require.NoError(t, err)

	reinstated, err := ReinstateAgent(conv)
	require.NoError(t, err)
	require.True(t, reinstated)
	n, err = ReenableGroupRetiredCronJobs(groupID)
	require.NoError(t, err)
	assert.Zero(t, n, "reinstatement/group resume cannot implicitly restore an owned retired job")
	job, err = GetAgentCronJob(jobID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.False(t, job.Enabled, "reinstatement must durably preserve the retired job's disabled state")
}

func TestExplicitCronDisableClearsAutomaticReasonAndSurvivesGroupResume(t *testing.T) {
	setupTestDB(t)
	groupID, err := CreateAgentGroup("explicit-disable", "")
	require.NoError(t, err)
	jobID, err := InsertAgentCronJob(&AgentCronJob{
		Name: "human-disabled", TargetKind: CronTargetGroup, GroupID: groupID,
		IntervalSeconds: 60, Body: "stay disabled", Enabled: true,
	})
	require.NoError(t, err)
	n, err := DisableGroupTargetCronJobsForRetire(groupID)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	disabled := false
	n, err = UpdateAgentCronJobFields(jobID, UpdateCronPatch{Enabled: &disabled})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	job, err := GetAgentCronJob(jobID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.False(t, job.Enabled)
	assert.Empty(t, job.DisabledReason, "explicit disable supersedes the automatic pause marker")

	n, err = ReenableGroupRetiredCronJobs(groupID)
	require.NoError(t, err)
	assert.Zero(t, n, "group resume must not resurrect a human-disabled job")
	job, err = GetAgentCronJob(jobID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.False(t, job.Enabled)
}

// pinCronTimes rewrites a job's created_at (and optionally last_run_at)
// directly, so due-logic tests control every timestamp the check reads and
// never race a wall-clock minute boundary.
func pinCronTimes(t *testing.T, id int64, created time.Time, lastRun *time.Time) {
	t.Helper()
	d, err := Open()
	require.NoError(t, err, "Open")
	mustExec(t, d, `UPDATE agent_cron_jobs SET created_at = '`+
		created.UTC().Format(time.RFC3339)+`' WHERE id = `+fmt.Sprint(id))
	if lastRun != nil {
		mustExec(t, d, `UPDATE agent_cron_jobs SET last_run_at = '`+
			lastRun.UTC().Format(time.RFC3339)+`' WHERE id = `+fmt.Sprint(id))
	}
}

func TestAgentCronJob_DueLogic_CronExpr(t *testing.T) {
	setupTestDB(t)

	// A minute-aligned base makes every "* * * * *" fire time exact: the
	// next fire after base is base+1m regardless of timezone (all tz
	// offsets are minute-aligned).
	base := time.Now().Truncate(time.Minute)

	// jNew: never run — anchors on created_at, so it is NOT due until the
	// first match after creation (unlike interval jobs, which fire at once).
	jNew, err := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		CronExpr: "* * * * *", Body: "x", Enabled: true,
	})
	require.NoError(t, err)
	pinCronTimes(t, jNew, base, nil)

	// jRan: last fired exactly at base — next match is base+1m.
	jRan, err := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		CronExpr: "* * * * *", Body: "x", Enabled: true,
	})
	require.NoError(t, err)
	ranAt := base
	pinCronTimes(t, jRan, base.Add(-time.Hour), &ranAt)

	// jOff: due by schedule but disabled.
	jOff, err := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		CronExpr: "* * * * *", Body: "x", Enabled: false,
	})
	require.NoError(t, err)
	pinCronTimes(t, jOff, base.Add(-time.Hour), nil)

	// jBad: an unparseable expression (only reachable by hand-editing the
	// row — write paths validate). Must be skipped, never abort the sweep.
	jBad, err := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		CronExpr: "* * * * *", Body: "x", Enabled: true,
	})
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `UPDATE agent_cron_jobs SET cron_expr = 'not an expr' WHERE id = `+fmt.Sprint(jBad))

	dueAt := func(now time.Time) map[int64]bool {
		due, err := ListDueAgentCronJobs(now)
		require.NoError(t, err, "ListDueAgentCronJobs")
		ids := map[int64]bool{}
		for _, j := range due {
			ids[j.ID] = true
		}
		return ids
	}

	// 30s after base: no minute boundary has passed since created/last-run.
	early := dueAt(base.Add(30 * time.Second))
	assert.False(t, early[jNew], "never-run expr job waits for its first match")
	assert.False(t, early[jRan], "ran-at-base job not due before the next match")

	// 90s after base: the base+1m match has passed for everyone.
	late := dueAt(base.Add(90 * time.Second))
	assert.True(t, late[jNew], "never-run expr job due after its first match")
	assert.True(t, late[jRan], "ran-at-base job due after the next match")
	assert.False(t, late[jOff], "disabled expr job never due")
	assert.False(t, late[jBad], "unparseable expr skipped, not fired")
}

func TestAgentCronJob_CronExpr_RoundTripAndPatch(t *testing.T) {
	setupTestDB(t)

	id, err := InsertAgentCronJob(&AgentCronJob{
		OwnerConv: "a", TargetConv: "b",
		CronExpr: "*/10 * * * *", Body: "x", Enabled: true,
	})
	require.NoError(t, err)

	got, err := GetAgentCronJob(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "*/10 * * * *", got.CronExpr, "cron_expr round-trips")
	assert.Equal(t, int64(0), got.IntervalSeconds, "expr job carries no interval")

	// Patch to interval mode: set the interval, clear the expression.
	n, err := UpdateAgentCronJobFields(id, UpdateCronPatch{
		IntervalSeconds: new(int64(300)), CronExpr: new(""),
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err = GetAgentCronJob(id)
	require.NoError(t, err)
	assert.Equal(t, "", got.CronExpr, "expr cleared")
	assert.Equal(t, int64(300), got.IntervalSeconds, "interval set")
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
