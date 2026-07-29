package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV168toV169CreatesStandingOrderTables(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v169?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (168)`)

	require.NoError(t, migrateV168toV169(d))
	assert.Equal(t, 169, schemaVersion(d))

	var emptyOrders, emptyDeliveries int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_standing_orders`).Scan(&emptyOrders))
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_standing_order_deliveries`).Scan(&emptyDeliveries))
	assert.Zero(t, emptyOrders, "the migration must not opt existing users into an order")
	assert.Zero(t, emptyDeliveries, "the migration must not synthesize delivery history")

	mustExec(t, d, `INSERT INTO agent_standing_orders
		(name, target_kind, group_id, summary, trigger_event, trigger_sources,
		 timing, cadence, created_at)
		VALUES ('pr-early', 'group', 1, 'push early', 'session.start', 'compact,startup',
		        'same-continuation', 'always', '2026-07-29T00:00:00Z')`)

	var revision int64
	var enabled, operator int
	require.NoError(t, d.QueryRow(
		`SELECT revision, enabled, operator_authored FROM agent_standing_orders WHERE name = 'pr-early'`,
	).Scan(&revision, &enabled, &operator))
	assert.Equal(t, int64(1), revision, "a new order starts at revision 1")
	assert.Equal(t, 1, enabled, "orders default to enabled")
	assert.Equal(t, 0, operator, "authorship is explicit, never assumed")

	mustExec(t, d, `INSERT INTO agent_standing_order_deliveries
		(order_id, order_revision, target_conv, epoch, outcome, created_at)
		VALUES (1, 1, 'conv-a', 'conv-a', 'delivered', '2026-07-29T00:00:01Z')`)
	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_standing_order_deliveries`).Scan(&n))
	assert.Equal(t, 1, n)

	require.NoError(t, migrateV168toV169(d), "partially applied migration converges")
}

// Names are the handle every CLI verb and the skill address an order by, so a
// duplicate must be refused at the schema level rather than only in Go.
func TestMigrateV168toV169EnforcesUniqueOrderName(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v169-unique?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (168)`)
	require.NoError(t, migrateV168toV169(d))

	const insert = `INSERT INTO agent_standing_orders
		(name, target_kind, group_id, summary, trigger_event, trigger_sources,
		 timing, cadence, created_at)
		VALUES ('dup', 'group', 1, 'x', 'session.start', '', 'next-turn', 'always', '2026-07-29T00:00:00Z')`
	mustExec(t, d, insert)
	_, err = d.Exec(insert)
	require.Error(t, err, "a duplicate order name must be refused")
}
