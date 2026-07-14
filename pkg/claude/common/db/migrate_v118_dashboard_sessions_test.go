package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV117toV118AddsDashboardSessionGrace(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v118?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (117);
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV117toV118(d))
	require.NoError(t, migrateV117toV118(d), "half-applied migration converges")

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 118, version)
	_, err = d.Exec(`INSERT INTO dashboard_session_grace (token_hash, expires_at, created_at)
		VALUES ('digest', 123, 'now')`)
	require.NoError(t, err)
}
