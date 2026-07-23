package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV149AddsOpenCodeRuntimeRegistry(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v149-opencode?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (148)`)

	require.NoError(t, migrateV148toV149(d))
	assert.Equal(t, 149, schemaVersion(d))
	require.NoError(t, migrateV148toV149(d), "migration is convergent")

	_, err = d.Exec(`INSERT INTO opencode_runtimes
		(session_id, conv_id, server_url, password, pid, cwd, created_at, updated_at)
		VALUES ('spwn-1', 'ses_1', 'http://127.0.0.1:1', 'secret', 42, '/tmp', 'now', 'now')`)
	require.NoError(t, err)
	var password string
	require.NoError(t, d.QueryRow(
		`SELECT password FROM opencode_runtimes WHERE session_id = 'spwn-1'`,
	).Scan(&password))
	assert.Equal(t, "secret", password)
}
