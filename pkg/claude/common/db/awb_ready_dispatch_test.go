package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAWBReadyDispatchLifecycleAndCAS(t *testing.T) {
	setupTestDB(t)
	selected, err := SelectAWBReadyDispatch("tcl", "tcl-a1", "agt_reserved")
	require.NoError(t, err)
	assert.True(t, selected)
	selected, err = SelectAWBReadyDispatch("tcl", "tcl-b2", "agt_other")
	require.NoError(t, err)
	assert.False(t, selected)
	d, err := GetAWBReadyDispatch("tcl")
	require.NoError(t, err)
	assert.Equal(t, "tcl-a1", d.IssueID)
	assert.Equal(t, "selected", d.Phase)
	ok, err := UpdateAWBReadyDispatch("tcl", "tcl-a1", "claimed", "retry")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = UpdateAWBReadyDispatch("tcl", "wrong", "spawned", "")
	require.NoError(t, err)
	assert.False(t, ok)
	dbConn, err := Open()
	require.NoError(t, err)
	_, err = dbConn.Exec(`UPDATE awb_ready_dispatches SET phase='invalid' WHERE workspace='tcl'`)
	assert.Error(t, err)
	require.NoError(t, SetAWBReadyDispatchAgent("tcl", "tcl-a1", "agt_live"))
	ok, err = ClearAWBReadyDispatch("tcl", "wrong")
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = ClearAWBReadyDispatch("tcl", "tcl-a1")
	require.NoError(t, err)
	assert.True(t, ok)
}
