package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A task's refs after the first are stored in order, read back with the
// task, replaced by an edit that records the change, and kept from the
// first ref's columns, which stay the one ref older readers know.
func TestGroupTaskMoreRefs(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	pr := GroupTaskRef{Kind: "github", URL: "https://github.com/o/r/pull/7", Label: "#7"}
	awb := GroupTaskRef{Kind: "awb", Label: "tcl-a1b2c3"}
	task, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "two refs", Actor: "human:operator",
		RefKind: "linear", RefURL: "https://linear.app/x/issue/LIN-1", RefLabel: "LIN-1", MoreRefs: []GroupTaskRef{pr, awb}})
	require.NoError(t, err)
	assert.Equal(t, "LIN-1", task.RefLabel)
	assert.Equal(t, []GroupTaskRef{pr, awb}, task.MoreRefs)
	assert.Equal(t, []GroupTaskRef{{Kind: "linear", URL: "https://linear.app/x/issue/LIN-1", Label: "LIN-1"}, pr, awb}, task.Refs())

	got, err := GetGroupTask(task.ID)
	require.NoError(t, err)
	assert.Equal(t, task.MoreRefs, got.MoreRefs)

	more := []GroupTaskRef{awb}
	edited, err := EditGroupTask(task.ID, GroupTaskEdit{MoreRefs: &more, Actor: "human:operator"})
	require.NoError(t, err)
	assert.Equal(t, []GroupTaskRef{awb}, edited.MoreRefs)
	events, err := ListGroupTaskEvents(task.ID)
	require.NoError(t, err)
	last := events[len(events)-1]
	assert.Equal(t, GroupTaskEventEdited, last.Kind)
	assert.Equal(t, "refs", last.Detail)
	require.Len(t, last.Changes, 1)
	assert.Equal(t, "more_refs", last.Changes[0].Field)

	// The same list again is no change.
	_, err = EditGroupTask(task.ID, GroupTaskEdit{MoreRefs: &more, Actor: "human:operator"})
	require.NoError(t, err)
	again, err := ListGroupTaskEvents(task.ID)
	require.NoError(t, err)
	assert.Len(t, again, len(events))

	none := []GroupTaskRef{}
	cleared, err := EditGroupTask(task.ID, GroupTaskEdit{MoreRefs: &none, Actor: "human:operator"})
	require.NoError(t, err)
	assert.Empty(t, cleared.MoreRefs)
	assert.Equal(t, "LIN-1", cleared.RefLabel)

	// A task without refs has none.
	plain, err := CreateGroupTask(NewGroupTask{GroupID: gid, Title: "plain", Actor: "human:operator"})
	require.NoError(t, err)
	assert.Empty(t, plain.Refs())
}
