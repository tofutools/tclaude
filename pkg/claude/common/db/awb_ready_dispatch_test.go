package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAWBReadyDispatchLifecycleAndCAS(t *testing.T) {
	setupTestDB(t)
	selected, err := SelectAWBReadyDispatch("builders", "tcl", "tcl-a1", "agt_reserved")
	require.NoError(t, err)
	assert.True(t, selected)
	selected, err = SelectAWBReadyDispatch("builders", "tcl", "tcl-b2", "agt_other")
	require.NoError(t, err)
	assert.False(t, selected)
	selected, err = SelectAWBReadyDispatch("duplicate", "tcl", "tcl-a1", "agt_duplicate")
	require.NoError(t, err)
	assert.False(t, selected, "two processes cannot reserve the same issue")
	selected, err = SelectAWBReadyDispatch("frontend", "tcl", "tcl-c3", "agt_frontend")
	require.NoError(t, err)
	assert.True(t, selected, "a separate process may dispatch independently in the same workspace")
	_, err = ClearAWBReadyDispatch("frontend", "tcl-c3")
	require.NoError(t, err)
	d, err := GetAWBReadyDispatch("builders")
	require.NoError(t, err)
	assert.Equal(t, "tcl-a1", d.IssueID)
	assert.Equal(t, "builders", d.Process)
	assert.Equal(t, "tcl", d.Workspace)
	assert.Equal(t, "selected", d.Phase)
	ok, err := UpdateAWBReadyDispatch("builders", "tcl-a1", "claimed", "retry")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = UpdateAWBReadyDispatch("builders", "wrong", "spawned", "")
	require.NoError(t, err)
	assert.False(t, ok)
	dbConn, err := Open()
	require.NoError(t, err)
	_, err = dbConn.Exec(`UPDATE awb_ready_dispatches SET phase='invalid' WHERE process='builders'`)
	assert.Error(t, err)
	require.NoError(t, SetAWBReadyDispatchAgent("builders", "tcl-a1", "agt_live"))
	ok, err = ClearAWBReadyDispatch("builders", "wrong")
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = ClearAWBReadyDispatch("builders", "tcl-a1")
	require.NoError(t, err)
	assert.True(t, ok)
}
