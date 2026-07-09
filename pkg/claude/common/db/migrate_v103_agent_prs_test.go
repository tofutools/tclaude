package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV102toV103_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 103, currentVersion, "tripwire: bump this and add a v103->v104 test when you add a migration")

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_prs'`).Scan(&have))
	assert.Equal(t, 1, have, "fresh schema has agent_prs")
}

func TestMigrateV102toV103_CreatesAgentPRs(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	mustExec(t, d, `DROP TABLE agent_prs`)
	mustExec(t, d, `UPDATE schema_version SET version = 102`)

	require.NoError(t, migrateV102toV103(d), "v102→v103")

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_prs'`).Scan(&have))
	assert.Equal(t, 1, have, "agent_prs created")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 103, ver, "version advanced")

	require.NoError(t, migrateV102toV103(d), "v102→v103 re-run is a clean no-op")
}

func TestAgentPR_UpsertDedupeAndHandled(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	_, _, err = EnsureAgentForConv("conv-a", "test")
	require.NoError(t, err)
	agentID, err := AgentIDForConv("conv-a")
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	_, _, err = EnsureAgentForConv("conv-b", "test")
	require.NoError(t, err)
	otherAgentID, err := AgentIDForConv("conv-b")
	require.NoError(t, err)
	require.NotEmpty(t, otherAgentID)

	first, err := UpsertAgentPR(agentID, "https://github.com/tofutools/tclaude/pull/7", "first", "open")
	require.NoError(t, err)
	second, err := UpsertAgentPR(agentID, "https://github.com/tofutools/tclaude/pull/7", "second", "merged")
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "same URL updates existing row")
	assert.Equal(t, "second", second.Summary)
	assert.Equal(t, "merged", second.State)
	other, err := UpsertAgentPR(otherAgentID, "https://github.com/tofutools/tclaude/pull/7", "other agent", "open")
	require.NoError(t, err)
	assert.NotEqual(t, second.ID, other.ID, "same URL is allowed for a different agent")

	rows, err := ListUnhandledAgentPRs()
	require.NoError(t, err)
	require.Len(t, rows[agentID], 1)
	require.Len(t, rows[otherAgentID], 1)

	n, err := MarkAgentPRHandled(agentID, "https://github.com/tofutools/tclaude/pull/7")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	rows, err = ListUnhandledAgentPRs()
	require.NoError(t, err)
	assert.Empty(t, rows[agentID], "handled PR is omitted")
	require.Len(t, rows[otherAgentID], 1, "handling one agent's PR leaves the other agent's presentation visible")
}
