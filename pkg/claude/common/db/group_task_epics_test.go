package db

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requireTaskConflict(t *testing.T, err error, code string) {
	t.Helper()
	var c *GroupTaskConflict
	require.True(t, errors.As(err, &c), "want conflict %s, got %v", code, err)
	require.Equal(t, code, c.Code, c.Msg)
}

func TestGroupTaskEpicLifecycle(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	other, err := CreateAgentGroup("other", "")
	require.NoError(t, err)
	worker, _, err := EnsureAgentForConv("conv-worker-1", "test")
	require.NoError(t, err)
	add := func(in NewGroupTask) *GroupTask {
		if in.GroupID == 0 {
			in.GroupID = gid
		}
		in.Actor = "human:operator"
		task, err := CreateGroupTask(in)
		require.NoError(t, err)
		return task
	}
	epic := add(NewGroupTask{Title: "Epic", Kind: GroupTaskKindEpic})
	require.True(t, epic.IsEpic())
	require.Equal(t, GroupTaskStageIdea, epic.Stage)

	// An epic has no assignee, column, parent, block.
	_, err = CreateGroupTask(NewGroupTask{GroupID: gid, Title: "x", Kind: GroupTaskKindEpic, AssigneeID: worker})
	requireTaskConflict(t, err, "invalid_arg")
	_, err = CreateGroupTask(NewGroupTask{GroupID: gid, Title: "x", Kind: GroupTaskKindEpic, ParentID: epic.ID})
	requireTaskConflict(t, err, "epic")
	_, err = AssignGroupTask(epic.ID, worker, "human:operator", false, "human:operator", AnyAssignee)
	requireTaskConflict(t, err, "epic")
	_, err = SetGroupTaskStatus(epic.ID, GroupTaskBlocked, "why", "human:operator")
	requireTaskConflict(t, err, "epic")

	// Children: same group only, and only under an epic.
	a := add(NewGroupTask{Title: "a", ParentID: epic.ID})
	b := add(NewGroupTask{Title: "b"})
	require.Equal(t, epic.ID, a.ParentID)
	_, err = CreateGroupTask(NewGroupTask{GroupID: other, Title: "x", ParentID: epic.ID})
	requireTaskConflict(t, err, "invalid_arg")
	_, err = CreateGroupTask(NewGroupTask{GroupID: gid, Title: "x", ParentID: b.ID})
	requireTaskConflict(t, err, "not_epic")
	_, err = EditGroupTask(epic.ID, GroupTaskEdit{ParentID: &epic.ID})
	requireTaskConflict(t, err, "epic")

	// Re-parenting is an event on the task and both epics.
	epic2 := add(NewGroupTask{Title: "Epic 2", Kind: GroupTaskKindEpic})
	b, err = EditGroupTask(b.ID, GroupTaskEdit{ParentID: &epic2.ID, Actor: "human:operator"})
	require.NoError(t, err)
	require.Equal(t, epic2.ID, b.ParentID)
	b, err = EditGroupTask(b.ID, GroupTaskEdit{ParentID: &epic.ID, Actor: "human:operator"})
	require.NoError(t, err)
	evKinds := func(id string) []string {
		evs, err := ListGroupTaskEvents(id)
		require.NoError(t, err)
		out := []string{}
		for _, e := range evs {
			out = append(out, e.Kind)
		}
		return out
	}
	require.Contains(t, evKinds(b.ID), GroupTaskEventParent)
	require.Contains(t, evKinds(epic2.ID), GroupTaskEventChildAdded)
	require.Contains(t, evKinds(epic2.ID), GroupTaskEventChildRemoved)

	// Filters and progress.
	tasks, err := ListGroupTasks(gid, GroupTaskListOpts{Parent: epic.ID})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a", "b"}, taskIDs(tasks))
	tasks, err = ListGroupTasks(gid, GroupTaskListOpts{Kind: GroupTaskKindEpic})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"Epic", "Epic 2"}, taskIDs(tasks))
	_, err = AssignGroupTask(a.ID, worker, "human:operator", false, "human:operator", AnyAssignee)
	require.NoError(t, err)
	prog, err := GroupTaskEpicProgresses([]string{epic.ID, epic2.ID})
	require.NoError(t, err)
	require.Equal(t, GroupTaskEpicProgress{Total: 2, InProgress: 1, Ideas: 1}, *prog[epic.ID])
	require.Nil(t, prog[epic2.ID])

	// Done is refused while children are open, unless forced.
	_, err = SetGroupTaskStatus(epic.ID, GroupTaskDone, "", "human:operator")
	requireTaskConflict(t, err, "epic_open_children")

	// The last child closing suggests closing the epic, once.
	_, err = ReportGroupTask(a.ID, worker, GroupTaskOutcomeSucceeded, "ok", "agent:"+worker)
	require.NoError(t, err)
	got, err := GetGroupTask(epic.ID)
	require.NoError(t, err)
	require.True(t, got.CloseSuggestedAt.IsZero(), "b is still open")
	_, err = SetGroupTaskStatus(b.ID, GroupTaskDropped, "not needed", "human:operator")
	require.NoError(t, err)
	got, err = GetGroupTask(epic.ID)
	require.NoError(t, err)
	require.False(t, got.CloseSuggestedAt.IsZero())
	ok, err := ClaimGroupTaskCloseNudge(epic.ID)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = ClaimGroupTaskCloseNudge(epic.ID)
	require.NoError(t, err)
	require.False(t, ok, "the nudge is sent once")
	prog, err = GroupTaskEpicProgresses([]string{epic.ID})
	require.NoError(t, err)
	require.Equal(t, GroupTaskEpicProgress{Total: 1, Done: 1, Dropped: 1}, *prog[epic.ID])

	// A child reopened takes the suggestion back.
	_, err = SetGroupTaskStatus(b.ID, GroupTaskOpen, "", "human:operator")
	require.NoError(t, err)
	got, _ = GetGroupTask(epic.ID)
	require.True(t, got.CloseSuggestedAt.IsZero())

	// Archive: an epic waits for its children, then goes with them.
	_, err = SetGroupTaskStatusForced(epic.ID, GroupTaskDone, "", "human:operator", AnyAssignee, true)
	require.NoError(t, err)
	_, err = ArchiveGroupTask(epic.ID, "human:operator")
	requireTaskConflict(t, err, "epic_open_children")
	ids, err := ArchiveClosedGroupTasks(gid, []string{GroupTaskDone}, time.Time{}, "human:operator")
	require.NoError(t, err)
	require.Equal(t, []string{a.ID}, ids, "the epic waits for b")
	_, err = SetGroupTaskStatus(b.ID, GroupTaskDone, "", "human:operator")
	require.NoError(t, err)
	_, err = ArchiveGroupTask(epic.ID, "human:operator")
	require.NoError(t, err)
	got, _ = GetGroupTask(b.ID)
	require.False(t, got.ArchivedAt.IsZero(), "b went with its epic")
	require.Equal(t, epic.ID, got.ParentID, "children keep their epic")
	// A child out of the archive brings its epic along.
	_, err = UnarchiveGroupTask(a.ID, "human:operator")
	require.NoError(t, err)
	got, _ = GetGroupTask(epic.ID)
	require.True(t, got.ArchivedAt.IsZero())

	// Purge: the epic outlives its children.
	_, purged, err := SweepGroupTasks(time.Time{}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 3, purged, "a, b and the then childless epic")
	_, err = GetGroupTask(epic.ID)
	require.ErrorIs(t, err, ErrGroupTaskNotFound)
}

func TestGroupTaskEpicDropReleasesChildren(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	epic, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "Epic", Kind: GroupTaskKindEpic, Actor: "op"})
	require.NoError(t, err)
	c, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "c", ParentID: epic.ID, Actor: "op"})
	require.NoError(t, err)
	_, err = SetGroupTaskStatus(epic.ID, GroupTaskDropped, "scrapped", "op")
	require.NoError(t, err)
	c, err = GetGroupTask(c.ID)
	require.NoError(t, err)
	require.Empty(t, c.ParentID)
	require.Equal(t, GroupTaskOpen, c.Status, "the work stays")
}

func TestGroupTaskEpicClosedTakesNoTasksAndReopensWithoutAssignee(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	worker, _, err := EnsureAgentForConv("conv-worker-1", "test")
	require.NoError(t, err)
	epic, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "Epic", Kind: GroupTaskKindEpic, Actor: "op"})
	require.NoError(t, err)
	_, err = SetGroupTaskStatus(epic.ID, GroupTaskDropped, "", "op")
	require.NoError(t, err)
	_, err = CreateGroupTask(NewGroupTask{GroupID: gid, Title: "late", ParentID: epic.ID, Actor: "op"})
	requireTaskConflict(t, err, "not_live")

	_, err = ReopenGroupTask(epic.ID, GroupTaskReopen{Assignee: worker, Actor: "op"})
	requireTaskConflict(t, err, "epic")
	got, err := ReopenGroupTask(epic.ID, GroupTaskReopen{Assignee: KeepAssignee, Actor: "op"})
	require.NoError(t, err)
	require.Equal(t, GroupTaskOpen, got.Status)
	require.Empty(t, got.AssigneeID)
}
