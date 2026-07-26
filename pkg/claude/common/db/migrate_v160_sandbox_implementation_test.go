package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV159toV160AddsSandboxImplementation(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN sandbox_implementation`)
	mustExec(t, d, `UPDATE schema_version SET version = 159`)
	mustExec(t, d, `INSERT INTO sessions (id, status, created_at, updated_at)
		VALUES ('legacy-session', 'idle', '2026-07-26T00:00:00Z', '2026-07-26T00:00:00Z')`)

	require.NoError(t, migrateV159toV160(d))

	var implementation string
	require.NoError(t, d.QueryRow(
		`SELECT sandbox_implementation FROM sessions WHERE id = 'legacy-session'`).Scan(&implementation))
	assert.Equal(t, "harness-builtin", implementation)
	assert.Equal(t, 160, schemaVersion(d))
	require.NoError(t, migrateV159toV160(d), "partially applied migration converges")
	assert.Equal(t, 160, currentVersion, "tripwire: bump this with the next migration")
}
