package db

import (
	"errors"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Inserting keys at random positions keeps them strictly ordered, never
// ending in the lowest digit.
func TestTaskRankBetweenKeepsOrder(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	keys := []string{}
	for range 2000 {
		i := r.IntN(len(keys) + 1)
		lo, hi := "", ""
		if i > 0 {
			lo = keys[i-1]
		}
		if i < len(keys) {
			hi = keys[i]
		}
		k := taskRankBetween(lo, hi)
		require.True(t, lo == "" || lo < k, "%q < %q", lo, k)
		require.True(t, hi == "" || k < hi, "%q < %q", k, hi)
		require.NotEqual(t, byte('0'), k[len(k)-1], k)
		keys = append(keys[:i], append([]string{k}, keys[i:]...)...)
	}
	assert.True(t, sort.StringsAreSorted(keys))
}

func taskIDs(tasks []*GroupTask) []string {
	out := []string{}
	for _, t := range tasks {
		out = append(out, t.Title)
	}
	return out
}

func TestGroupTaskBoardOrderAndLifecycle(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	add := func(title, stage string, pos GroupTaskPosition, deps ...string) *GroupTask {
		task, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: title, Stage: stage, Position: pos,
			DependsOn: deps, Actor: "human:operator"})
		require.NoError(t, err)
		return task
	}
	list := func(opts GroupTaskListOpts) []string {
		tasks, err := ListGroupTasks(gid, opts)
		require.NoError(t, err)
		return taskIDs(tasks)
	}
	a := add("a", "", GroupTaskPosition{})
	assert.Equal(t, GroupTaskStageIdea, a.Stage, "capture lands in ideas")
	b := add("b", GroupTaskStageNext, GroupTaskPosition{})
	c := add("c", GroupTaskStageNext, GroupTaskPosition{Top: true})
	d := add("d", "", GroupTaskPosition{Before: b.ID}, b.ID)
	assert.Equal(t, GroupTaskStageNext, d.Stage, "a position takes the anchor's stage")
	assert.Equal(t, []string{b.ID}, d.DependsOn)
	assert.Equal(t, []string{"c", "d", "b", "a"}, list(GroupTaskListOpts{}))

	// A move across the divider demotes in the same write.
	_, err = MoveGroupTask(c.ID, GroupTaskPosition{After: a.ID}, "human:operator")
	require.NoError(t, err)
	assert.Equal(t, []string{"d", "b", "a", "c"}, list(GroupTaskListOpts{}))
	assert.Equal(t, []string{"a", "c"}, list(GroupTaskListOpts{Stage: GroupTaskStageIdea}))
	// Promote goes to the end of the queue.
	_, err = SetGroupTaskStage(a.ID, GroupTaskStageNext, "human:operator")
	require.NoError(t, err)
	assert.Equal(t, []string{"d", "b", "a", "c"}, list(GroupTaskListOpts{}))

	// A cycle is refused.
	_, err = EditGroupTask(b.ID, GroupTaskEdit{AddDeps: []string{d.ID}, Actor: "human:operator"})
	var conflict *GroupTaskConflict
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "dep_cycle", conflict.Code)

	// Done leaves the live list, stays under Closed; reopen puts it back where it was.
	_, err = SetGroupTaskStatus(b.ID, GroupTaskDone, "", "human:operator")
	require.NoError(t, err)
	assert.Equal(t, []string{"d", "a", "c"}, list(GroupTaskListOpts{}))
	assert.Equal(t, []string{"d", "a", "c", "b"}, list(GroupTaskListOpts{Closed: true}))
	_, err = SetGroupTaskStatus(b.ID, GroupTaskOpen, "", "human:operator")
	require.NoError(t, err)
	assert.Equal(t, []string{"d", "b", "a", "c"}, list(GroupTaskListOpts{}))

	// Block needs a live task; history records each change.
	_, err = SetGroupTaskStatus(b.ID, GroupTaskBlocked, "waiting on CI", "human:operator")
	require.NoError(t, err)
	events, err := ListGroupTaskEvents(b.ID)
	require.NoError(t, err)
	kinds := []string{}
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	assert.Equal(t, []string{"created", "done", "reopened", "blocked"}, kinds)

	// The sweep archives, then purges.
	_, err = SetGroupTaskStatus(c.ID, GroupTaskDropped, "parked in awb", "human:operator")
	require.NoError(t, err)
	n, _, err := SweepGroupTasks(time.Now().Add(time.Minute), time.Time{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.NotContains(t, list(GroupTaskListOpts{Closed: true}), "c")
	_, purged, err := SweepGroupTasks(time.Time{}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.EqualValues(t, 1, purged)
	_, err = GetGroupTask(c.ID)
	assert.True(t, errors.Is(err, ErrGroupTaskNotFound))
}
