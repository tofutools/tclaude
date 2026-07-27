package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV163toV164GrandfathersOnlyLayeredOpenCodeAgents(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `DROP TABLE opencode_agent_state_allocations`)
	mustExec(t, d, `UPDATE schema_version SET version = 163`)

	for _, row := range []struct {
		agent, conv, harness, implementation string
	}{
		{"agt_aaaaaaaa", "conv-layered", "opencode", "tclaude-layer"},
		{"agt_bbbbbbbb", "conv-builtin", "opencode", "harness-builtin"},
		{"agt_cccccccc", "conv-codex", "codex", "tclaude-layer"},
	} {
		mustExec(t, d, `INSERT INTO agents
			(agent_id, current_conv_id, created_at) VALUES (?, ?, 'now')`,
			row.agent, row.conv)
		mustExec(t, d, `INSERT INTO agent_conversations
			(conv_id, agent_id, role, linked_at) VALUES (?, ?, 'head', 'now')`,
			row.conv, row.agent)
		mustExec(t, d, `INSERT INTO sessions
			(id, conv_id, harness, sandbox_implementation, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'now', 'now')`,
			"session-"+row.conv, row.conv, row.harness, row.implementation)
	}
	mustExec(t, d, `INSERT INTO agents
		(agent_id, current_conv_id, created_at) VALUES
		('agt_dddddddd', 'conv-profile-only', 'now'),
		('agt_eeeeeeee', 'conv-profile-builtin', 'now')`)
	mustExec(t, d, `INSERT INTO agent_conversations
		(conv_id, agent_id, role, linked_at) VALUES
		('conv-profile-only', 'agt_dddddddd', 'head', 'now'),
		('conv-profile-builtin', 'agt_eeeeeeee', 'head', 'now')`)
	mustExec(t, d, `INSERT INTO conversation_resume_profiles
		(conv_id, profile_json, updated_at) VALUES
		('conv-profile-only',
		 '{"version":1,"harness":"opencode","fallback_relaunch":{"version":1,"sandbox_implementation":"tclaude-layer"}}',
		 'now'),
		('conv-profile-builtin',
		 '{"version":1,"harness":"opencode","fallback_relaunch":{"version":1,"sandbox_implementation":"harness-builtin"}}',
		 'now')`)

	require.NoError(t, migrateV163toV164(d))
	assert.Equal(t, 164, schemaVersion(d))
	for _, agentID := range []string{"agt_aaaaaaaa", "agt_dddddddd"} {
		allocation, err := GetOpenCodeAgentStateAllocation(agentID)
		require.NoError(t, err)
		require.NotNil(t, allocation)
		assert.Equal(t, OpenCodeStateLegacyShared, allocation.Mode)
		assert.Empty(t, allocation.StateRoot)
	}
	for _, agentID := range []string{
		"agt_bbbbbbbb", "agt_cccccccc", "agt_eeeeeeee",
	} {
		allocation, err := GetOpenCodeAgentStateAllocation(agentID)
		require.NoError(t, err)
		assert.Nil(t, allocation)
	}
	require.NoError(t, migrateV163toV164(d), "partially applied migration converges")
}
