package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDarwinRouteLaunchIsExactGenerationBound(t *testing.T) {
	setupTestDB(t)
	agentID := NewAgentID()
	require.NoError(t, RegisterDarwinRouteLaunch(agentID, "conv-a", "gen-a", []int{41001, 41002}))
	launch, err := GetDarwinRouteLaunch(agentID, "conv-a", "gen-a")
	require.NoError(t, err)
	require.Equal(t, DarwinRouteLaunchPending, launch.State)
	require.Equal(t, []int{41001, 41002}, launch.Slots)
	require.NoError(t, ActivateDarwinRouteLaunch(agentID, "conv-a", "gen-a"))
	launch, err = GetDarwinRouteLaunch(agentID, "conv-a", "gen-a")
	require.NoError(t, err)
	require.Equal(t, DarwinRouteLaunchActive, launch.State)

	// A relaunch generation cannot observe or mutate its predecessor's
	// contract, even when the stable agent and conversation are unchanged.
	_, err = GetDarwinRouteLaunch(agentID, "conv-a", "gen-b")
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.Error(t, ActivateDarwinRouteLaunch(agentID, "conv-a", "gen-b"))

	d, err := Open()
	require.NoError(t, err)
	tx, err := d.Begin()
	require.NoError(t, err)
	require.NoError(t, MarkDarwinRouteLaunchClosedTx(tx, agentID, "conv-a", "gen-a", time.Now()))
	require.NoError(t, tx.Commit())
	launch, err = GetDarwinRouteLaunch(agentID, "conv-a", "gen-a")
	require.NoError(t, err)
	require.Equal(t, DarwinRouteLaunchClosed, launch.State)
}
