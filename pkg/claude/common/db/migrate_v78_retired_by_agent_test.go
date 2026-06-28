package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV77toV78_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts the retired_by_agent companion column is present. v78 is
// head, so the literal currentVersion tripwire lives here now (moved forward
// from v77).
func TestMigrateV77toV78_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 78, currentVersion, "tripwire: bump this and add a v78→v79 test when you add a migration")

	has, err := columnExists(d, "agents", "retired_by_agent")
	require.NoError(t, err, "columnExists agents.retired_by_agent")
	assert.True(t, has, "agents carries the retired_by_agent companion column")
}

// TestMigrateV77toV78_BackfillsRetiredByAgent drives the real v77→v78 migration
// over hand-seeded v77-shaped agents rows: a row retired by an AGENT (retired_by
// = that retirer's conv-id) backfills retired_by_agent to the retirer's stable
// actor id; a row retired by a LITERAL ("human") stays empty; and a re-run changes
// nothing. This pins the same agent_conversations derivation the write-time
// dual-write uses, so backfilled and freshly written rows agree.
func TestMigrateV77toV78_BackfillsRetiredByAgent(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// retirer is the actor that performed the retire; targetA/targetH are the
	// retired actors (by an agent, and by a human literal, respectively).
	retirer, err := AllocateAgent("retirer-conv", "spawn")
	require.NoError(t, err)
	targetA, err := AllocateAgent("targetA-conv", "spawn")
	require.NoError(t, err)
	targetH, err := AllocateAgent("targetH-conv", "spawn")
	require.NoError(t, err)

	// Reshape the agents table back to its v77 (pre-companion) form and pin the
	// version, then seed the retire audit in that shape with a raw UPDATE (the
	// companion column is gone, so RetireAgentByID can't be used here).
	for _, s := range []string{
		`ALTER TABLE agents DROP COLUMN retired_by_agent`,
		`UPDATE schema_version SET version = 77`,
	} {
		mustExec(t, d, s)
	}
	mustExec(t, d, `UPDATE agents SET retired_at = '2020-01-01T00:00:00Z', retired_by = 'retirer-conv' WHERE agent_id = '`+targetA+`'`)
	mustExec(t, d, `UPDATE agents SET retired_at = '2020-01-02T00:00:00Z', retired_by = 'human' WHERE agent_id = '`+targetH+`'`)

	require.NoError(t, migrateV77toV78(d), "v77→v78 backfill")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 78, ver, "version advanced")

	var got string
	require.NoError(t, d.QueryRow(`SELECT retired_by_agent FROM agents WHERE agent_id = ?`, targetA).Scan(&got))
	assert.Equal(t, retirer, got, "an agent retirer's conv-id backfills to its stable actor id")

	require.NoError(t, d.QueryRow(`SELECT retired_by_agent FROM agents WHERE agent_id = ?`, targetH).Scan(&got))
	assert.Equal(t, "", got, "a literal retired_by ('human') leaves the companion empty")

	// Idempotent: a re-run recomputes the same join and changes nothing.
	require.NoError(t, migrateV77toV78(d), "v77→v78 re-run is a clean no-op")
	require.NoError(t, d.QueryRow(`SELECT retired_by_agent FROM agents WHERE agent_id = ?`, targetA).Scan(&got))
	assert.Equal(t, retirer, got, "re-run leaves the backfilled companion intact")
}

// TestRetireDualWritesRetiredByAgent pins db.RetireAgentByID deriving the
// retirer's stable agent_id from `by` at write time (JOH-306): an agent retirer
// (by = its conv-id) lands its agent_id in retired_by_agent; a literal ("human")
// leaves it empty. Reinstate clears the companion alongside the rest of the audit.
func TestRetireDualWritesRetiredByAgent(t *testing.T) {
	setupTestDB(t)
	_, err := Open()
	require.NoError(t, err, "Open")

	retirer, err := AllocateAgent("retirer-conv", "spawn")
	require.NoError(t, err)
	targetA, err := AllocateAgent("targetA-conv", "spawn")
	require.NoError(t, err)
	targetH, err := AllocateAgent("targetH-conv", "spawn")
	require.NoError(t, err)

	// An agent performed the retire: `by` is the retirer's conv-id.
	ok, err := RetireAgentByID(targetA, "retirer-conv", "cleanup")
	require.NoError(t, err)
	require.True(t, ok)
	a, _ := GetAgent(targetA)
	require.NotNil(t, a)
	assert.Equal(t, "retirer-conv", a.RetiredBy, "raw audit value preserved")
	assert.Equal(t, retirer, a.RetiredByAgent, "companion derived from the retirer's conv-id")

	// A human performed the retire: the literal leaves the companion empty.
	ok, err = RetireAgentByID(targetH, "human", "cleanup")
	require.NoError(t, err)
	require.True(t, ok)
	a, _ = GetAgent(targetH)
	require.NotNil(t, a)
	assert.Equal(t, "human", a.RetiredBy)
	assert.Equal(t, "", a.RetiredByAgent, "a human retirer leaves the companion empty")

	// Reinstate clears the companion alongside retired_at / retired_by.
	ok, err = ReinstateAgentByID(targetA)
	require.NoError(t, err)
	require.True(t, ok)
	a, _ = GetAgent(targetA)
	require.NotNil(t, a)
	assert.True(t, a.Active())
	assert.Equal(t, "", a.RetiredBy, "reinstate clears the raw value")
	assert.Equal(t, "", a.RetiredByAgent, "reinstate clears the companion")
}
