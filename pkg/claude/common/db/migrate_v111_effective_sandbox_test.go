package db

import (
	"os"
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

func TestEffectiveSandboxBookkeepingSurvivesDeletedGrantDirectory(t *testing.T) {
	setupTestDB(t)
	grantDir := t.TempDir()
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name:       "ephemeral",
		Filesystem: []sandboxpolicy.FilesystemGrant{{Path: grantDir, Access: sandboxpolicy.AccessWrite}},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)

	require.NoError(t, SaveSession(&SessionRow{ID: "stale", ConvID: "conv-stale", EffectiveSandbox: &snapshot}))
	require.NoError(t, SaveSession(&SessionRow{ID: "healthy", ConvID: "conv-healthy"}))
	require.NoError(t, InsertPendingSpawn(&PendingSpawn{Label: "stale", GroupID: 1, EffectiveSandbox: &snapshot}))
	require.NoError(t, InsertPendingSpawn(&PendingSpawn{Label: "healthy", GroupID: 1}))
	require.NoError(t, os.RemoveAll(grantDir))

	sessions, err := ListSessions()
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	var staleSession *SessionRow
	for _, row := range sessions {
		if row.ID == "stale" {
			staleSession = row
		}
	}
	require.NotNil(t, staleSession)
	require.NotNil(t, staleSession.EffectiveSandbox)
	assert.True(t, reflect.DeepEqual(snapshot, *staleSession.EffectiveSandbox))
	require.NoError(t, SaveSession(staleSession), "hook-style bookkeeping updates must not require live grant paths")

	pending, err := ListPendingSpawns()
	require.NoError(t, err)
	require.Len(t, pending, 2)
	var stalePending *PendingSpawn
	for _, row := range pending {
		if row.Label == "stale" {
			stalePending = row
		}
	}
	require.NotNil(t, stalePending)
	require.NotNil(t, stalePending.EffectiveSandbox)
	assert.True(t, reflect.DeepEqual(snapshot, *stalePending.EffectiveSandbox))
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

func TestEffectiveSandboxSnapshotUpgradesLegacyVersionAtPersistenceBoundary(t *testing.T) {
	legacy := sandboxpolicy.EmptySnapshot()
	legacy.Version = 1

	raw, err := marshalEffectiveSandboxSnapshot(&legacy)
	require.NoError(t, err)
	decoded, err := unmarshalEffectiveSandboxSnapshot(raw)
	require.NoError(t, err)
	require.NotNil(t, decoded)
	assert.Equal(t, sandboxpolicy.SnapshotVersion, decoded.Version)

	// Existing database rows were written before marshal knew about v2, so
	// exercise the read side against a literal v1 payload too.
	decoded, err = unmarshalEffectiveSandboxSnapshot(`{"version":1,"effective":{},"applied":[]}`)
	require.NoError(t, err)
	require.NotNil(t, decoded)
	assert.Equal(t, sandboxpolicy.SnapshotVersion, decoded.Version)
}
