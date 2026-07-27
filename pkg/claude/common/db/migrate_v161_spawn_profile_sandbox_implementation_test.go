package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pre-existing profile pinned nothing, and must keep pinning nothing. The
// column therefore backfills to "" (unset — falls through to the next spawn
// precedence tier), NOT to 'harness-builtin' the way the sessions column does:
// that value would turn every legacy profile into one that OVERRIDES lower
// tiers. See migrateV160toV161's doc comment.
func TestMigrateV160toV161AddsSpawnProfileSandboxImplementation(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN sandbox_implementation`)
	mustExec(t, d, `UPDATE schema_version SET version = 160`)
	mustExec(t, d, `INSERT INTO spawn_profiles (name, created_at, updated_at)
		VALUES ('legacy-profile', '2026-07-26T00:00:00Z', '2026-07-26T00:00:00Z')`)

	require.NoError(t, migrateV160toV161(d))

	var implementation string
	require.NoError(t, d.QueryRow(
		`SELECT sandbox_implementation FROM spawn_profiles WHERE name = 'legacy-profile'`).Scan(&implementation))
	assert.Equal(t, "", implementation, "a legacy profile must stay unset, not pin harness-builtin")
	assert.Equal(t, 161, schemaVersion(d))
	require.NoError(t, migrateV160toV161(d), "partially applied migration converges")
	assert.GreaterOrEqual(t, currentVersion, 161)
}
