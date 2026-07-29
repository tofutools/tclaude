package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV172toV173CreatesStandingOrderTurnOrigins(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v173?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (172);
		CREATE TABLE agent_messages (id INTEGER PRIMARY KEY);
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV172toV173(d))
	assert.Equal(t, 173, schemaVersion(d))

	var have int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('agent_standing_order_turn_origins')
		 WHERE name IN ('target_agent', 'message_id', 'state', 'armed_at', 'expires_at')
	`).Scan(&have))
	assert.Equal(t, 5, have)
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('agent_standing_order_messages')
		 WHERE name IN ('message_id', 'order_id', 'order_revision')
	`).Scan(&have))
	assert.Equal(t, 3, have)
	require.NoError(t, migrateV172toV173(d), "already-applied migration is a no-op")
}
