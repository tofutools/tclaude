package db

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroupTaskAssignReportAndRetire(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	worker, _, err := EnsureAgentForConv("conv-worker-1", "test")
	require.NoError(t, err)
	other, _, err := EnsureAgentForConv("conv-other-2", "test")
	require.NoError(t, err)

	idea, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "idea", Actor: "t"})
	require.NoError(t, err)
	// A take needs a queued task; an assign promotes an idea.
	_, err = AssignGroupTask(idea.ID, worker, "agent:"+worker, true, "agent:"+worker, AnyAssignee)
	var conflict *GroupTaskConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "not_queued", conflict.Code)
	task, err := AssignGroupTask(idea.ID, worker, "human:operator", false, "human:operator", AnyAssignee)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageNext, task.Stage)
	assert.Equal(t, worker, task.AssigneeID)

	// Only the assignee reports.
	_, err = ReportGroupTask(idea.ID, other, GroupTaskOutcomeSucceeded, "", "agent:"+other)
	assert.True(t, errors.Is(err, ErrGroupTaskNotAssignee))

	// Failures unassign, and the third blocks.
	for i := 1; i <= GroupTaskFailureLimit; i++ {
		_, err = AssignGroupTask(idea.ID, worker, "human:operator", false, "human:operator", AnyAssignee)
		require.NoError(t, err, "attempt %d", i)
		rep, err := ReportGroupTask(idea.ID, worker, GroupTaskOutcomeFailed, "flaky", "agent:"+worker)
		require.NoError(t, err)
		assert.Empty(t, rep.Task.AssigneeID)
		assert.Equal(t, i, rep.Task.Failures)
		assert.Equal(t, i == GroupTaskFailureLimit, rep.Blocked)
		// The same report again is acknowledged, not counted.
		again, err := ReportGroupTask(idea.ID, worker, GroupTaskOutcomeFailed, "flaky", "agent:"+worker)
		require.NoError(t, err)
		assert.True(t, again.Repeated)
		assert.Equal(t, i, again.Task.Failures)
	}
	assert.Equal(t, GroupTaskBlocked, func() string { g, _ := GetGroupTask(idea.ID); return g.Status }())

	// done closes the task and keeps the assignee recorded.
	next, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "next", Stage: GroupTaskStageNext, Actor: "t"})
	require.NoError(t, err)
	_, err = AssignGroupTask(next.ID, worker, "agent:"+worker, true, "agent:"+worker, AnyAssignee)
	require.NoError(t, err)
	rep, err := ReportGroupTask(next.ID, worker, GroupTaskOutcomeSucceeded, "merged", "agent:"+worker)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskDone, rep.Task.Status)
	assert.Equal(t, worker, rep.Task.AssigneeID)
	rep, err = ReportGroupTask(next.ID, worker, GroupTaskOutcomeSucceeded, "merged", "agent:"+worker)
	require.NoError(t, err)
	assert.True(t, rep.Repeated)

	// Retiring the assignee unassigns its live tasks.
	live, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "live", Stage: GroupTaskStageNext, Actor: "t"})
	require.NoError(t, err)
	_, err = AssignGroupTask(live.ID, other, "human:operator", false, "human:operator", AnyAssignee)
	require.NoError(t, err)
	mine, err := ListGroupTasksAssignedTo(other)
	require.NoError(t, err)
	require.Len(t, mine, 1)
	assert.Equal(t, "crew", mine[0].GroupName)
	ok, err := RetireAgentByID(other, "human", "test")
	require.NoError(t, err)
	require.True(t, ok)
	after, err := GetGroupTask(live.ID)
	require.NoError(t, err)
	assert.Empty(t, after.AssigneeID)
	events, err := ListGroupTaskEvents(live.ID)
	require.NoError(t, err)
	last := events[len(events)-1]
	assert.Equal(t, GroupTaskEventUnassigned, last.Kind)
	assert.Equal(t, "assignee retired", last.Detail)

	// The lock round-trips; nil unlocks.
	require.NoError(t, SetTaskboardLock(gid, []string{worker, worker}, "human:operator"))
	lock, err := GetTaskboardLock(gid)
	require.NoError(t, err)
	assert.True(t, lock.Locked)
	assert.Equal(t, []string{worker}, lock.Writers)
	require.NoError(t, SetTaskboardLock(gid, nil, "human:operator"))
	lock, err = GetTaskboardLock(gid)
	require.NoError(t, err)
	assert.False(t, lock.Locked)
}

// Unblocking after the failure limit gives the task a fresh run of attempts.
func TestGroupTaskUnblockResetsFailures(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	worker, _, err := EnsureAgentForConv("conv-worker-9", "test")
	require.NoError(t, err)
	task, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "t", Stage: GroupTaskStageNext, Actor: "t"})
	require.NoError(t, err)
	for i := range GroupTaskFailureLimit {
		_, err = AssignGroupTask(task.ID, worker, "human:operator", false, "human:operator", AnyAssignee)
		require.NoError(t, err)
		_, err = ReportGroupTask(task.ID, worker, GroupTaskOutcomeFailed, string(rune('a'+i)), "agent:"+worker)
		require.NoError(t, err)
	}
	open, err := SetGroupTaskStatus(task.ID, GroupTaskOpen, "", "human:operator")
	require.NoError(t, err)
	assert.Zero(t, open.Failures)
	_, err = AssignGroupTask(task.ID, worker, "human:operator", false, "human:operator", AnyAssignee)
	require.NoError(t, err)
	rep, err := ReportGroupTask(task.ID, worker, GroupTaskOutcomeFailed, "again", "agent:"+worker)
	require.NoError(t, err)
	assert.False(t, rep.Blocked)
	// A changed-hands check refuses.
	_, err = UnassignGroupTask(task.ID, false, "", "human:operator", worker)
	var conflict *GroupTaskConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "not_assigned", conflict.Code)
}
