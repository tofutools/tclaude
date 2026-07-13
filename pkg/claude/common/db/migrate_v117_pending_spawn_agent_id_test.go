package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV116toV117AddsPendingSpawnAgentID(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v117?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (116);
		CREATE TABLE pending_spawns (
			label TEXT PRIMARY KEY,
			group_id INTEGER NOT NULL,
			created_at TEXT NOT NULL
		);
		INSERT INTO pending_spawns (label, group_id, created_at) VALUES ('legacy', 1, 'now');
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV116toV117(d))
	require.NoError(t, migrateV116toV117(d), "half-applied migration converges")

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 117, version)
	var legacy string
	require.NoError(t, d.QueryRow(`SELECT agent_id FROM pending_spawns WHERE label = 'legacy'`).Scan(&legacy))
	assert.Empty(t, legacy, "legacy pending rows keep the allocate-on-enrollment fallback")
	var launching int
	require.NoError(t, d.QueryRow(`SELECT launching FROM pending_spawns WHERE label = 'legacy'`).Scan(&launching))
	assert.Zero(t, launching, "legacy rows are not marked as actively launching")
	_, err = d.Exec(`INSERT INTO pending_spawns (label, group_id, created_at, agent_id)
		VALUES ('one', 1, 'now', 'agt_reserved'), ('two', 1, 'now', 'agt_reserved')`)
	require.Error(t, err, "non-empty pending agent ids are unique")
}
