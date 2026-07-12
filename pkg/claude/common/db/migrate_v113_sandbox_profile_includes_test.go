package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV112toV113AddsSandboxProfileIncludes(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	var have int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'includes_json'`).Scan(&have))
	assert.Equal(t, 1, have)

	mustExec(t, d, `UPDATE schema_version SET version = 112`)
	require.NoError(t, migrateV112toV113(d))
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 113, version)

	// Pre-existing rows fall back to the empty-array default and load cleanly.
	mustExec(t, d, `INSERT INTO sandbox_profiles (name, created_at, updated_at) VALUES ('legacy', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	p, err := GetSandboxProfile("legacy")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.NotNil(t, p.Includes)
	assert.Empty(t, p.Includes)
}
