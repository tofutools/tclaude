package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV154toV155AddsContextFeatureColumns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN context_features`)
	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN context_features`)
	mustExec(t, d, `UPDATE schema_version SET version = 154`)
	mustExec(t, d, `INSERT INTO spawn_profiles (name, created_at, updated_at)
		VALUES ('legacy-profile', 1784883600000000000, 1784883600000000000)`)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status, created_at, updated_at)
		VALUES ('legacy-session', 'tc-legacy', 0, '/tmp', 'conv-legacy', 'idle',
		        1784883600000000000, 1784883600000000000)`)

	require.NoError(t, migrateV154toV155(d))

	// A legacy row reads as "no trims", which is the untrimmed startup context
	// every pre-v155 agent actually launched with.
	var profileFeatures, sessionFeatures string
	require.NoError(t, d.QueryRow(
		`SELECT context_features FROM spawn_profiles WHERE name = 'legacy-profile'`).Scan(&profileFeatures))
	require.NoError(t, d.QueryRow(
		`SELECT context_features FROM sessions WHERE id = 'legacy-session'`).Scan(&sessionFeatures))
	assert.Empty(t, profileFeatures)
	assert.Empty(t, sessionFeatures)

	assert.Equal(t, 155, schemaVersion(d))
	require.NoError(t, migrateV154toV155(d), "partially applied migration converges")
}
