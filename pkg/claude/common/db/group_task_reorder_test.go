package db

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpacedTaskRanksAreOrderedAndShort(t *testing.T) {
	for _, n := range []int{0, 1, 2, 35, 36, 500, 2000} {
		keys := spacedTaskRanks(n)
		require.Len(t, keys, n)
		assert.True(t, sort.StringsAreSorted(keys), "n=%d", n)
		for i, k := range keys {
			require.NotEmpty(t, k)
			require.NotEqual(t, byte('0'), k[len(k)-1], k)
			require.LessOrEqual(t, len(k), 4, "n=%d", n)
			if i > 0 {
				// Room for a key between each two without growing past the width.
				require.LessOrEqual(t, len(taskRankBetween(keys[i-1], k)), 4)
			}
		}
	}
}

// A long run of moves between the same neighbours ("move up" clicked again
// and again) keeps every key short and the order intact, closed tasks too.
func TestMoveGroupTaskRenumbersLongKeys(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	var ids []string
	for _, title := range []string{"a", "b", "c", "d"} {
		task, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: title, Stage: GroupTaskStageNext, Actor: "human:operator"})
		require.NoError(t, err)
		ids = append(ids, task.ID)
	}
	closed, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "z", Stage: GroupTaskStageNext, Actor: "human:operator"})
	require.NoError(t, err)
	_, err = SetGroupTaskStatus(closed.ID, GroupTaskDone, "", "human:operator")
	require.NoError(t, err)

	// Swap the first two over and over, each time just before the other.
	for i := range 400 {
		mover, anchor := ids[1], ids[0]
		if i%2 == 1 {
			mover, anchor = ids[0], ids[1]
		}
		_, err := MoveGroupTask(mover, GroupTaskPosition{Before: anchor, Stage: GroupTaskStageNext}, "human:operator")
		require.NoError(t, err)
	}
	tasks, err := ListGroupTasks(gid, GroupTaskListOpts{Closed: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c", "d", "z"}, taskIDs(tasks), "400 swaps leave a before b; the closed task keeps its place last")
	for _, task := range tasks {
		assert.LessOrEqual(t, len(task.Rank), taskRankMaxLen, task.Title)
	}
}

// A move that expects a stage is refused, not carried across, when its
// anchor sits in the other one.
func TestMoveGroupTaskStageGuard(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	next, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "n", Stage: GroupTaskStageNext, Actor: "human:operator"})
	require.NoError(t, err)
	idea, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "i", Stage: GroupTaskStageIdea, Actor: "human:operator"})
	require.NoError(t, err)

	_, err = MoveGroupTask(next.ID, GroupTaskPosition{Before: idea.ID, Stage: GroupTaskStageNext}, "human:operator")
	var conflict *GroupTaskConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "stale", conflict.Code)
	got, err := GetGroupTask(next.ID)
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageNext, got.Stage, "a refused move changes nothing")

	_, err = MoveGroupTask(next.ID, GroupTaskPosition{Top: true, Stage: GroupTaskStageIdea}, "human:operator")
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "stale", conflict.Code, "top of its own stage is not the stage the caller saw")

	// Without the guard the move still crosses, as before.
	moved, err := MoveGroupTask(next.ID, GroupTaskPosition{Before: idea.ID}, "human:operator")
	require.NoError(t, err)
	assert.Equal(t, GroupTaskStageIdea, moved.Stage)
}

// A burst of moves by one actor is one activity entry; another actor's move,
// or one after the window, adds its own.
func TestMoveGroupTaskCoalescesActivity(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	a, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "a", Stage: GroupTaskStageNext, Actor: "human:operator"})
	require.NoError(t, err)
	b, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "b", Stage: GroupTaskStageNext, Actor: "human:operator"})
	require.NoError(t, err)
	moves := func() []*GroupTaskEvent {
		events, err := ListGroupTaskEvents(b.ID)
		require.NoError(t, err)
		var out []*GroupTaskEvent
		for _, e := range events {
			if e.Kind == GroupTaskEventMoved {
				out = append(out, e)
			}
		}
		return out
	}
	for range 5 {
		_, err := MoveGroupTask(b.ID, GroupTaskPosition{Top: true}, "human:operator")
		require.NoError(t, err)
		_, err = MoveGroupTask(b.ID, GroupTaskPosition{After: a.ID}, "human:operator")
		require.NoError(t, err)
	}
	got := moves()
	require.Len(t, got, 1, "ten moves in a row by the operator are one entry")
	assert.Equal(t, "after "+a.ID, got[0].Detail, "the entry says where the last move put it")

	_, err = MoveGroupTask(b.ID, GroupTaskPosition{Top: true}, "agent:ag_x")
	require.NoError(t, err)
	assert.Len(t, moves(), 2, "another actor's move is its own entry")

	// Age the latest entry past the window: the next move adds one.
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE group_task_events SET at = ? WHERE task_id = ?`, dbTime(time.Now().Add(-2*taskMovedCoalesce)), b.ID)
	require.NoError(t, err)
	_, err = MoveGroupTask(b.ID, GroupTaskPosition{After: a.ID}, "agent:ag_x")
	require.NoError(t, err)
	assert.Len(t, moves(), 3, "a move after the window is a new entry")
}
