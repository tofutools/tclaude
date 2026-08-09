package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV200AddsCopilotModelTierColumns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, setSchemaVersion(d, 199))
	mustExec(t, d, `ALTER TABLE copilot_model_catalog DROP COLUMN enriched_json`)
	mustExec(t, d, `ALTER TABLE copilot_model_catalog DROP COLUMN long_context_max_prompt_tokens`)

	require.NoError(t, migrateV199toV200(d))
	assert.Equal(t, 200, schemaVersion(d))
	for _, name := range []string{"long_context_max_prompt_tokens", "enriched_json"} {
		var count int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('copilot_model_catalog') WHERE name = ?`, name,
		).Scan(&count))
		assert.Equal(t, 1, count, name)
	}
}

func TestMigrateV200SurvivesMinimalHealSchema(t *testing.T) {
	d, err := sql.Open("sqlite", t.TempDir()+"/minimal.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (199)`)

	require.NoError(t, migrateV199toV200(d))
	assert.Equal(t, 200, schemaVersion(d))
	var count int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('copilot_model_catalog')`,
	).Scan(&count))
	assert.GreaterOrEqual(t, count, 8)
}
