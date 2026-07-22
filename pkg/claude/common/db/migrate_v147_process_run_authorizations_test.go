package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV147AddsProcessAuthorizationInPlace(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v147-process-authorizations?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (145)`)
	require.NoError(t, migrateV145toV146(d))
	mustExec(t, d, `INSERT INTO process_runs
		(id, template_ref, template_snapshot_json, params_json, status, state_version,
		 checkpoint_json, created_at, updated_at)
		VALUES ('run_old', 't@sha256:x', '{}', '{}', 'running', 1, '{}', 'now', 'now')`)

	require.NoError(t, migrateV146toV147(d))
	assert.Equal(t, 147, schemaVersion(d))
	require.NoError(t, migrateV146toV147(d), "repeated migration converges")

	var authorizations string
	require.NoError(t, d.QueryRow(`SELECT program_authorizations_json FROM process_runs WHERE id = 'run_old'`).Scan(&authorizations))
	assert.Equal(t, `[]`, authorizations, "existing runs fail closed")
	_, err = d.Exec(`UPDATE process_runs SET program_authorizations_json = '' WHERE id = 'run_old'`)
	assert.Error(t, err, "authorization JSON has a non-empty bounded envelope")
}
