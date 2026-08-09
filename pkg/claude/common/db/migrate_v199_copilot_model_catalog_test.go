package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV198toV199AddsCopilotModelCatalog(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `DROP TABLE copilot_model_catalog`)
	mustExec(t, d, `UPDATE schema_version SET version = 198`)

	require.NoError(t, migrateV198toV199(d))
	assert.Equal(t, 199, schemaVersion(d))
	var strict int
	require.NoError(t, d.QueryRow(`SELECT strict FROM pragma_table_list
		WHERE name = 'copilot_model_catalog'`).Scan(&strict))
	assert.Equal(t, 1, strict)
	require.NoError(t, migrateV198toV199(d), "partially applied migration converges")
}

func TestMigrateV198toV199SurvivesMinimalHealSchema(t *testing.T) {
	d, err := sql.Open("sqlite", t.TempDir()+"/minimal.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (198)`)

	require.NoError(t, migrateV198toV199(d))
	assert.Equal(t, 199, schemaVersion(d))
}
