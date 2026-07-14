package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV118toV119AddsAgentdIdempotency(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v119?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (118);
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV118toV119(d))
	require.NoError(t, migrateV118toV119(d), "half-applied migration converges")

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 119, version)
	_, err = d.Exec(`INSERT INTO agentd_idempotency
		(request_key, fingerprint, owner_id, state, created_at, expires_at)
		VALUES ('key', 'fingerprint', 'owner', 'pending', 1, 2)`)
	require.NoError(t, err)
}
