package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionExecutionBoundaryRoundTrip(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, SaveSession(&SessionRow{ID: "spwn-boundary", ConvID: "conv-boundary"}))
	require.NoError(t, SetSessionExecutionBoundary("spwn-boundary", `{"version":1,"path":{"before_pre_launch":"/.tclaude/bin:/usr/bin"}}`))
	raw, err := SessionExecutionBoundary("spwn-boundary")
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"path":{"before_pre_launch":"/.tclaude/bin:/usr/bin"}}`, raw)

	missing, err := SessionExecutionBoundary("missing")
	require.NoError(t, err)
	assert.Empty(t, missing)
}

func TestSessionExecutionBoundaryLaunchCASRejectsSuccessorAndExitedRows(t *testing.T) {
	setupTestDB(t)
	const (
		sessionID  = "spwn-boundary-cas"
		generation = "11111111111111111111111111111111"
		paneID     = "%7"
	)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, ConvID: "conv-boundary", TmuxSession: "tmux-boundary",
		Status: "working", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, SetSessionExitLaunchGeneration(sessionID, generation))
	require.NoError(t, SetSessionExitLaunchBinding(sessionID, generation, strings.Repeat("a", 64), paneID))

	stored, err := SetSessionExecutionBoundaryForLaunch(
		sessionID, generation, "tmux-boundary", paneID, `{"generation":"first"}`)
	require.NoError(t, err)
	require.True(t, stored)

	const successor = "22222222222222222222222222222222"
	require.NoError(t, SetSessionExitLaunchGeneration(sessionID, successor))
	require.NoError(t, SetSessionExitLaunchBinding(sessionID, successor, strings.Repeat("b", 64), "%8"))
	stored, err = SetSessionExecutionBoundaryForLaunch(
		sessionID, generation, "tmux-boundary", paneID, `{"generation":"stale"}`)
	require.NoError(t, err)
	assert.False(t, stored)
	raw, err := SessionExecutionBoundary(sessionID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"generation":"first"}`, raw)

	row, err := LoadSession(sessionID)
	require.NoError(t, err)
	marked, err := MarkSessionExitedIfUnchanged(sessionID, row.Status, row.UpdatedAt, "unexpected")
	require.NoError(t, err)
	require.True(t, marked)
	stored, err = SetSessionExecutionBoundaryForLaunch(
		sessionID, successor, "tmux-boundary", "%8", `{"generation":"exited"}`)
	require.NoError(t, err)
	assert.False(t, stored)
}

func TestClearSessionExecutionBoundaryForLaunchIsSuccessorFenced(t *testing.T) {
	setupTestDB(t)
	const (
		sessionID     = "spwn-boundary-clear"
		first         = "11111111111111111111111111111111"
		successor     = "22222222222222222222222222222222"
		firstBoundary = "{\"version\":1}"
	)
	require.NoError(t, SaveSession(&SessionRow{
		ID: sessionID, TmuxSession: "tmux-boundary-clear", Status: "working",
		CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, SetSessionExitLaunchGeneration(sessionID, first))
	require.NoError(t, SetSessionExitLaunchBinding(
		sessionID, first, strings.Repeat("a", 64), "%7"))
	stored, err := SetSessionExecutionBoundaryForLaunch(
		sessionID, first, "tmux-boundary-clear", "%7", firstBoundary)
	require.NoError(t, err)
	require.True(t, stored)
	require.NoError(t, SetSessionExitLaunchGeneration(sessionID, successor))
	require.NoError(t, SetSessionExitLaunchBinding(
		sessionID, successor, strings.Repeat("b", 64), "%8"))

	cleared, err := ClearSessionExecutionBoundaryForLaunch(
		sessionID, first, "tmux-boundary-clear", "%7")
	require.NoError(t, err)
	assert.False(t, cleared, "a delayed predecessor rollback cannot erase successor evidence")
	boundary, err := SessionExecutionBoundary(sessionID)
	require.NoError(t, err)
	assert.Equal(t, firstBoundary, boundary)
	cleared, err = ClearSessionExecutionBoundaryForLaunch(
		sessionID, successor, "tmux-boundary-clear", "%8")
	require.NoError(t, err)
	assert.True(t, cleared)
	boundary, err = SessionExecutionBoundary(sessionID)
	require.NoError(t, err)
	assert.Empty(t, boundary)
}
