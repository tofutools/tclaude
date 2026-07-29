package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV169toV170AddsCooldownAndStableRecipientLedgerKey(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v170?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (168);`)
	require.NoError(t, err)
	require.NoError(t, migrateV168toV169(d))

	require.NoError(t, migrateV169toV170(d))
	assert.Equal(t, 170, schemaVersion(d))

	var cooldownDefault, targetAgentDefault string
	require.NoError(t, d.QueryRow(
		`SELECT dflt_value FROM pragma_table_info('agent_standing_orders')
		 WHERE name = 'cooldown_seconds'`,
	).Scan(&cooldownDefault))
	require.NoError(t, d.QueryRow(
		`SELECT dflt_value FROM pragma_table_info('agent_standing_order_deliveries')
		 WHERE name = 'target_agent'`,
	).Scan(&targetAgentDefault))
	assert.Equal(t, "0", cooldownDefault)
	assert.Equal(t, "''", targetAgentDefault)

	require.NoError(t, migrateV169toV170(d), "partially applied migration converges")
}
