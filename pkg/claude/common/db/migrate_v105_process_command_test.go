package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV104toV105AddsProcessCommandMetadata(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, migrateV104toV105(d), "migration is idempotent at a healed head schema")
	for _, table := range []string{"agents", "pending_spawns"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = 'process_command_id'`).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
	for _, column := range []string{"process_run_id", "process_node_id", "process_command_id"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('human_messages') WHERE name = ?`, column).Scan(&count))
		assert.Equal(t, 1, count, column)
	}
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 105, version)
}

func TestAgentProcessCommandBindingIsUniqueAndDiscoverable(t *testing.T) {
	setupTestDB(t)
	first, _, err := EnsureAgentForConv("proc-first", "test")
	require.NoError(t, err)
	second, _, err := EnsureAgentForConv("proc-second", "test")
	require.NoError(t, err)
	require.NoError(t, SetAgentProcessCommand(first, "cmd_aaaaaaaaaaaaaaaaaaaaaaaa"))
	assert.Error(t, SetAgentProcessCommand(second, "cmd_aaaaaaaaaaaaaaaaaaaaaaaa"))
	got, err := AgentForProcessCommand("cmd_aaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, first, got.AgentID)
	require.NoError(t, ClearAgentProcessCommandForConv("proc-first"))
	got, err = AgentForProcessCommand("cmd_aaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	assert.Nil(t, got)
}
