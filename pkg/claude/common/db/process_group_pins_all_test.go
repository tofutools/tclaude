package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListAllProcessGroupPinsInPinOrder(t *testing.T) {
	setupTestDB(t)
	crew, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	side, err := CreateAgentGroup("side", "")
	require.NoError(t, err)
	require.NoError(t, PinProcessToGroup(crew, "tpl_b"))
	require.NoError(t, PinProcessToGroup(crew, "tpl_a"))
	require.NoError(t, PinProcessToGroup(side, "tpl_c"))
	all, err := ListAllProcessGroupPins()
	require.NoError(t, err)
	assert.Equal(t, map[int64][]string{crew: {"tpl_b", "tpl_a"}, side: {"tpl_c"}}, all)
	runs, err := ListActiveGroupProcessRunSummaries(crew, MaxProcessRunReadPage)
	require.NoError(t, err)
	assert.Empty(t, runs)
}

func TestListAllProcessGroupPinTimes(t *testing.T) {
	setupTestDB(t)
	crew, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	before := time.Now().Add(-time.Second)
	require.NoError(t, PinProcessToGroup(crew, "tpl_a"))
	times, err := ListAllProcessGroupPinTimes()
	require.NoError(t, err)
	at := times[crew]["tpl_a"]
	assert.False(t, at.Before(before), "pinned_at %v predates the pin", at)
	assert.False(t, at.After(time.Now().Add(time.Second)), "pinned_at %v is in the future", at)
	assert.Empty(t, times[crew+1])
}
