package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV109toV110CreatesSandboxProfileStore(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	for table, columns := range map[string][]string{
		"sandbox_profiles":                  {"id", "name", "filesystem_json", "environment_json", "created_at", "updated_at"},
		"sandbox_profile_global_assignment": {"id", "profile_name", "profile_id"},
		"agent_groups":                      {"sandbox_profile", "sandbox_profile_id"},
	} {
		for _, column := range columns {
			var have int
			require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&have))
			assert.Equal(t, 1, have, "%s.%s", table, column)
		}
	}

	// The migration is idempotent after an interrupted version bump: all DDL
	// uses IF NOT EXISTS / guarded columns and converges on rerun.
	mustExec(t, d, `UPDATE schema_version SET version = 109`)
	require.NoError(t, migrateV109toV110(d))
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 110, version)
}
