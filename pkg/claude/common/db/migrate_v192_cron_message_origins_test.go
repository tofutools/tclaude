package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV191ToV192CreatesCronMessageOrigins(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	_, err = d.Exec(`DROP TABLE agent_cron_messages; UPDATE schema_version SET version = 191`)
	require.NoError(t, err)
	require.NoError(t, migrateV191toV192(d))
	require.NoError(t, migrateV191toV192(d), "migration is idempotent")
	assert.Equal(t, 192, schemaVersion(d))

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_cron_messages'`,
	).Scan(&have))
	assert.Equal(t, 1, have)
}
