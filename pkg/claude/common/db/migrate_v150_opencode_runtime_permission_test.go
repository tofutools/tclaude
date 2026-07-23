package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV150AddsOpenCodeRuntimePermission(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v150-opencode?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (149)`)
	mustExec(t, d, `CREATE TABLE opencode_runtimes (
		session_id TEXT PRIMARY KEY, conv_id TEXT NOT NULL DEFAULT '',
		server_url TEXT NOT NULL, password TEXT NOT NULL,
		pid INTEGER NOT NULL DEFAULT 0, cwd TEXT NOT NULL,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	)`)

	require.NoError(t, migrateV149toV150(d))
	assert.Equal(t, 150, schemaVersion(d))
	require.NoError(t, migrateV149toV150(d), "migration is convergent")

	_, err = d.Exec(`INSERT INTO opencode_runtimes
		(session_id, server_url, password, cwd, permission_json, created_at, updated_at)
		VALUES ('spwn-1', 'http://127.0.0.1:1', 'secret', '/tmp', '[{}]', 'now', 'now')`)
	require.NoError(t, err)
	var policy string
	require.NoError(t, d.QueryRow(
		`SELECT permission_json FROM opencode_runtimes WHERE session_id = 'spwn-1'`,
	).Scan(&policy))
	assert.Equal(t, "[{}]", policy)
}
