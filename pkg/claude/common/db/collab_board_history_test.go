package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollabBoardHistoryKeepsBoundedSteps(t *testing.T) {
	setupTestDB(t)
	_, err := CreateCollabBoard("plan", "Plan", "operator", 10)
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	count := func() (steps int) {
		t.Helper()
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM collab_board_history`).Scan(&steps))
		return steps
	}
	step := func(i int, before string) *CollabBoardHistory {
		return &CollabBoardHistory{Actor: "operator", Steps: [][]CollabBoardStepElement{
			{{ID: fmt.Sprintf("e%d", i), Before: json.RawMessage(before)}},
		}}
	}
	element := func(i int) []CollabBoardElement {
		return []CollabBoardElement{{ID: fmt.Sprintf("e%d", i), Version: 2 + int64(i), Body: json.RawMessage(`{}`)}}
	}
	for i := range collabBoardHistoryMaxSteps + 5 {
		_, _, err := ApplyCollabBoardEdit("plan", "operator", "", element(i), step(i, "null"), 1000, 1000)
		require.NoError(t, err)
	}
	assert.Equal(t, collabBoardHistoryMaxSteps, count(), "the oldest steps go")

	_, _, err = ApplyCollabBoardEdit("plan", "operator", "", element(0), step(0, "null"), 1000, 1000)
	require.NoError(t, err)
	assert.Equal(t, collabBoardHistoryMaxSteps, count(), "an edit the merge did not accept records nothing")

	huge := `"` + strings.Repeat("x", collabBoardHistoryMaxBytes) + `"`
	_, _, err = ApplyCollabBoardEdit("plan", "operator", "", []CollabBoardElement{{ID: "e0", Version: 1000, Body: json.RawMessage(`{}`)}}, step(0, huge), 1000, 1000)
	require.NoError(t, err)
	assert.Equal(t, 1, count(), "a step past the byte bound still stays, alone")

	_, _, err = ApplyCollabBoardEdit("plan", "operator", "", element(1000), step(1000, "null"), 1000, 1000)
	require.NoError(t, err)
	require.NoError(t, DeleteCollabBoard("plan"))
	assert.Zero(t, count(), "history goes with its board")
}
