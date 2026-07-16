package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV79toV80_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts the pin_gen column is present. (The currentVersion tripwire
// has moved forward to the head migration's test.)
func TestMigrateV79toV80_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	has, err := columnExists(d, "agent_messages", "pin_gen")
	require.NoError(t, err, "columnExists agent_messages.pin_gen")
	assert.True(t, has, "agent_messages carries the pin_gen column")
}

// TestMigrateV79toV80_AddsColumn drives the real v79→v80 migration over a
// v79-shaped DB (pin_gen dropped, version pinned back). It asserts the column is
// added, defaults every existing row to 0 (head-following — the historical
// behaviour), advances the version, and is idempotent on re-run.
func TestMigrateV79toV80_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Reshape agent_messages back to its v79 (pre-pin_gen) form and pin the
	// version, then seed a row with a raw INSERT in that shape. Both the
	// pin_gen column and the to_agent index are v80 artifacts, so drop both.
	for _, s := range []string{
		`DROP INDEX IF EXISTS idx_agent_messages_to_agent`,
		`DROP INDEX IF EXISTS idx_agent_messages_regular_agent_backlog`,
		`ALTER TABLE agent_messages DROP COLUMN pin_gen`,
		`UPDATE schema_version SET version = 79`,
	} {
		mustExec(t, d, s)
	}
	mustExec(t, d, `INSERT INTO agent_messages
		(id, group_id, from_conv, to_conv, subject, body, created_at)
		VALUES (1, 0, 'sender-conv', 'to-conv', '', 'hi', '2020-01-01T00:00:00Z')`)

	has, err := columnExists(d, "agent_messages", "pin_gen")
	require.NoError(t, err)
	require.False(t, has, "pin_gen is absent in the v79 shape")

	require.NoError(t, migrateV79toV80(d), "v79→v80")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 80, ver, "version advanced")

	has, err = columnExists(d, "agent_messages", "pin_gen")
	require.NoError(t, err)
	assert.True(t, has, "pin_gen added")

	var pinGen int
	require.NoError(t, d.QueryRow(`SELECT pin_gen FROM agent_messages WHERE id = 1`).Scan(&pinGen))
	assert.Equal(t, 0, pinGen, "existing row defaults to 0 (head-following)")

	var idxCount int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_agent_messages_to_agent'`,
	).Scan(&idxCount))
	assert.Equal(t, 1, idxCount, "to_agent index created")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op.
	require.NoError(t, migrateV79toV80(d), "v79→v80 re-run is a clean no-op")
}
