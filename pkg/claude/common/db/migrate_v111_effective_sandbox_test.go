package db

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestMigrateV110toV111AddsEffectiveSandboxSnapshots(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	for _, table := range []string{"agents", "pending_spawns"} {
		var have int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'effective_sandbox_config'`, table,
		).Scan(&have))
		assert.Equal(t, 1, have, table)
	}

	// A rerun after an interrupted version bump converges without duplicate
	// column errors, while old rows retain the fail-closed empty sentinel.
	mustExec(t, d, `UPDATE schema_version SET version = 110`)
	require.NoError(t, migrateV110toV111(d))
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 111, version)
}

func TestEffectiveSandboxSnapshotRoundTripsAgentAndPendingSpawn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Applied = []sandboxpolicy.AppliedProfile{{
		Scope: sandboxpolicy.ScopeGlobal, ID: 7, Name: "base", UpdatedAt: time.Unix(123, 0).UTC(),
	}}
	require.NoError(t, InsertPendingSpawn(&PendingSpawn{
		Label: "pending-one", GroupID: 1, EffectiveSandbox: &snapshot,
	}))
	pending, err := GetPendingSpawn("pending-one")
	require.NoError(t, err)
	require.NotNil(t, pending.EffectiveSandbox)
	assert.True(t, reflect.DeepEqual(snapshot, *pending.EffectiveSandbox))

	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at) VALUES ('agt_snap', 'conv-snap', 'now')`)
	mustExec(t, d, `INSERT INTO agent_conversations (conv_id, agent_id, linked_at) VALUES ('conv-snap', 'agt_snap', 'now')`)
	require.NoError(t, SetAgentEffectiveSandboxConfig("agt_snap", &snapshot))
	got, err := AgentEffectiveSandboxConfigForConv("conv-snap")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, reflect.DeepEqual(snapshot, *got))

	missing, err := AgentEffectiveSandboxConfigForConv("conv-missing")
	require.NoError(t, err)
	assert.Nil(t, missing)
}
