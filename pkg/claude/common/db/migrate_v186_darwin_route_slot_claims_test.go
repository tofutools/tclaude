package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrateV185toV186BackfillsLiveLaunchClaims(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `DROP TABLE darwin_route_slot_claims`)
	mustExec(t, d, `UPDATE schema_version SET version = 185`)
	mustExec(t, d, `INSERT INTO darwin_route_launches
		(agent_id, conv_id, launch_generation, slots, state, created_at)
		VALUES ('agent-backfill', 'conv-backfill', 'generation-backfill', '42201,42202', 'active', 1767225600000000000)`)
	require.NoError(t, migrateV185toV186(d))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM darwin_route_slot_claims
		WHERE agent_id = 'agent-backfill' AND conv_id = 'conv-backfill' AND state = 'active'`).Scan(&count))
	require.Equal(t, 2, count)
}
