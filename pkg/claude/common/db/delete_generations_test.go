package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteAgentByConvID_PredecessorKeepsLiveActor guards the JOH-26 delete
// semantics: identity is actor-level, so deleting a PAST conversation
// generation (a reincarnate / Claude Code /clear leaves the old conv around)
// must NOT wipe the live actor's memberships, permissions or actor row — only
// the live generation's delete tears the actor down.
func TestDeleteAgentByConvID_PredecessorKeepsLiveActor(t *testing.T) {
	setupTestDB(t)

	groupID, err := CreateAgentGroup("alpha", "")
	require.NoError(t, err, "CreateAgentGroup")

	// One actor, two generations: old → new (new is the live head).
	_, _, err = EnsureAgentForConv("old", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, _, err = RotateAgentConv("old", "new", "reincarnate")
	require.NoError(t, err, "RotateAgentConv")

	// Actor-level identity: a membership and a permission override.
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: "new", Role: "lead"}))
	require.NoError(t, GrantAgentPermission("new", "self.compact", "test"))

	actor, err := AgentIDForConv("new")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// --- Delete the PREDECESSOR generation. ---
	counts, err := DeleteAgentByConvID("old")
	require.NoError(t, err, "delete predecessor")
	assert.Zero(t, counts.GroupMembers, "predecessor delete must not touch actor-level memberships")
	assert.Zero(t, counts.Permissions, "predecessor delete must not touch actor-level permissions")

	// The actor and its identity survive; only the old generation is unlinked.
	still, err := GetAgent(actor)
	require.NoError(t, err)
	require.NotNil(t, still, "live actor survives a predecessor delete")
	assert.Equal(t, "new", still.CurrentConvID, "live conv pointer is unchanged")

	oldA, err := AgentIDForConv("old")
	require.NoError(t, err)
	assert.Empty(t, oldA, "the predecessor generation is unlinked")
	newA, err := AgentIDForConv("new")
	require.NoError(t, err)
	assert.Equal(t, actor, newA, "the live generation still resolves to the actor")

	members, err := ListAgentGroupMembers(groupID)
	require.NoError(t, err)
	assert.Len(t, members, 1, "the actor keeps its membership after a predecessor delete")

	// --- Delete the LIVE generation → the actor is fully torn down. ---
	counts, err = DeleteAgentByConvID("new")
	require.NoError(t, err, "delete live generation")
	assert.Equal(t, int64(1), counts.GroupMembers, "live-generation delete removes the actor's membership")

	gone, err := GetAgent(actor)
	require.NoError(t, err)
	assert.Nil(t, gone, "deleting the live generation removes the actor")

	members, err = ListAgentGroupMembers(groupID)
	require.NoError(t, err)
	assert.Empty(t, members, "no memberships remain once the actor is gone")
}

// TestDeleteAgentByConvID_CronJobsActorScoped guards the JOH-26 PR3a delete
// move: cron jobs are agent-keyed (owner_agent / target_agent), so they are
// torn down with the ACTOR (its current-generation delete), not on a
// predecessor delete — and the agent_cron_runs FK cascade still fires on the
// live schema (the RENAME-COLUMN migration preserved the FK).
func TestDeleteAgentByConvID_CronJobsActorScoped(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// One actor, two generations: old → new (new is the live head).
	_, _, err = EnsureAgentForConv("old", "spawn")
	require.NoError(t, err, "EnsureAgentForConv")
	_, _, err = RotateAgentConv("old", "new", "reincarnate")
	require.NoError(t, err, "RotateAgentConv")
	actor, err := AgentIDForConv("new")
	require.NoError(t, err)
	require.NotEmpty(t, actor)

	// A self-nudge owned by the actor, and a manager's job that TARGETS the
	// actor — both keyed (after resolution) on the actor's agent_id.
	selfJob, err := InsertAgentCronJob(&AgentCronJob{
		Name: "self-nudge", OwnerConv: "new", TargetKind: CronTargetConv,
		TargetConv: "new", IntervalSeconds: 600, Body: "check", Enabled: true,
	})
	require.NoError(t, err, "insert self job")
	mgrJob, err := InsertAgentCronJob(&AgentCronJob{
		Name: "mgr-ping", OwnerConv: "mgr", TargetKind: CronTargetConv,
		TargetConv: "new", IntervalSeconds: 600, Body: "status?", Enabled: true,
	})
	require.NoError(t, err, "insert mgr job")
	// A run row on each, so we can prove the FK cascade fires (or doesn't).
	for _, id := range []int64{selfJob, mgrJob} {
		_, err := InsertAgentCronRun(&AgentCronRun{JobID: id, FiredAt: time.Now().UTC(), Status: "ok"})
		require.NoError(t, err, "insert run")
	}
	countRuns := func() int {
		var n int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_cron_runs`).Scan(&n))
		return n
	}
	require.Equal(t, 2, countRuns(), "two run rows seeded")

	// --- Delete the PREDECESSOR generation: jobs + runs must be untouched. ---
	counts, err := DeleteAgentByConvID("old")
	require.NoError(t, err, "delete predecessor")
	assert.Zero(t, counts.CronJobsOwned, "predecessor delete leaves owned jobs")
	assert.Zero(t, counts.CronJobsTarget, "predecessor delete leaves targeting jobs")
	for _, id := range []int64{selfJob, mgrJob} {
		j, err := GetAgentCronJob(id)
		require.NoError(t, err)
		assert.NotNil(t, j, "job %d survives a predecessor delete", id)
	}
	assert.Equal(t, 2, countRuns(), "run history survives a predecessor delete")

	// --- Delete the LIVE generation: the actor's owned + targeting jobs go,
	// and their runs cascade-delete. ---
	counts, err = DeleteAgentByConvID("new")
	require.NoError(t, err, "delete live generation")
	assert.Equal(t, int64(1), counts.CronJobsOwned, "the actor's owned job is removed")
	assert.Equal(t, int64(1), counts.CronJobsTarget, "the job targeting the actor is removed")
	for _, id := range []int64{selfJob, mgrJob} {
		j, err := GetAgentCronJob(id)
		require.NoError(t, err)
		assert.Nil(t, j, "job %d is gone once the actor is torn down", id)
	}
	assert.Zero(t, countRuns(), "cron runs cascade-delete with their jobs (FK preserved)")
}
