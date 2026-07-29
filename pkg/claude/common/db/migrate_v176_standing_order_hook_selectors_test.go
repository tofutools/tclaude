package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV175toV176AddsEmptyHookSelectorTable(t *testing.T) {
	d, err := sql.Open("sqlite",
		"file:migrate-v176?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (175);
		CREATE TABLE agent_standing_orders (id INTEGER PRIMARY KEY);
		INSERT INTO agent_standing_orders(id) VALUES (11);
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV175toV176(d))
	assert.Equal(t, 176, schemaVersion(d))

	var count int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM agent_standing_order_hook_selectors`).Scan(&count))
	assert.Zero(t, count, "existing orders must retain legacy trigger behavior")

	require.NoError(t, migrateV175toV176(d), "migration must be idempotent")
}
