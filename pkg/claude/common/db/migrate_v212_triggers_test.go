package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV211toV212SeedsExistingPRsWithoutReplay(t *testing.T) {
	setupTestDB(t)
	agent, _, err := EnsureAgentForConv("conv-existing-pr", "test")
	require.NoError(t, err)
	row, err := UpsertAgentPR(agent, "https://github.com/o/r/pull/1", "old", "open")
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM trigger_pr_events`)
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE schema_version SET version=211`)
	require.NoError(t, err)
	require.NoError(t, migrateV211toV212(d))
	var status string
	require.NoError(t, d.QueryRow(`SELECT status FROM trigger_pr_events WHERE agent_pr_id=?`, row.ID).Scan(&status))
	assert.Equal(t, TriggerEventPreexisting, status)
}

func TestUpsertAgentPRQueuesOneDurableOpenEdge(t *testing.T) {
	setupTestDB(t)
	agent, _, err := EnsureAgentForConv("conv-new-pr", "test")
	require.NoError(t, err)
	_, err = UpsertAgentPRDetails(agent, "https://github.com/o/r/pull/42", "new", "open", "topic", false)
	require.NoError(t, err)
	_, err = UpsertAgentPRDetails(agent, "https://github.com/o/r/pull/42", "edited", "open", "topic-2", false)
	require.NoError(t, err)
	events, err := ListPendingTriggerPREvents(10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, 42, events[0].PRNumber)
	assert.Equal(t, "topic-2", events[0].PRBranch)
}
