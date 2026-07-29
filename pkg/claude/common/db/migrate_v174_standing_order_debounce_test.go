package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV173toV174AddsStandingOrderDebounce(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v174?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (173);
		CREATE TABLE agent_standing_orders (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL DEFAULT ''
		);
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV173toV174(d))
	assert.Equal(t, 174, schemaVersion(d))

	var have int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('agent_standing_orders')
		 WHERE name = 'debounce_seconds'
	`).Scan(&have))
	assert.Equal(t, 1, have)
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('agent_standing_order_debounce')
		 WHERE name IN ('order_id', 'order_revision', 'target_agent',
			'target_conv', 'epoch', 'harness', 'detail', 'due_at',
			'max_due_at', 'updated_at')
	`).Scan(&have))
	assert.Equal(t, 10, have)

	require.NoError(t, migrateV173toV174(d),
		"already-applied migration is a no-op")
}
