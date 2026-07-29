package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV174toV175AddsStandingOrderGroupScopes(t *testing.T) {
	d, err := sql.Open("sqlite",
		"file:migrate-v175?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (174);
		CREATE TABLE agent_groups (id INTEGER PRIMARY KEY);
		CREATE TABLE agent_standing_orders (id INTEGER PRIMARY KEY);
		INSERT INTO agent_groups(id) VALUES (7);
		INSERT INTO agent_standing_orders(id) VALUES (11);
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV174toV175(d))
	assert.Equal(t, 175, schemaVersion(d))

	_, err = d.Exec(`
		INSERT INTO agent_standing_order_group_scopes
			(order_id, group_id, created_at)
		VALUES (11, 7, '2026-07-29T00:00:00Z')`)
	require.NoError(t, err)

	var count int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM agent_standing_order_group_scopes
		 WHERE order_id = 11 AND group_id = 7`).Scan(&count))
	assert.Equal(t, 1, count)

	_, err = d.Exec(`DELETE FROM agent_groups WHERE id = 7`)
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM agent_standing_order_group_scopes`).Scan(&count))
	assert.Zero(t, count, "group deletion cascades reusable scope assignments")

	require.NoError(t, migrateV174toV175(d),
		"already-applied migration is a no-op")
}
