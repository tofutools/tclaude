package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV151toV152OpenCodeUsageActivity(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'opencode_usage_activity'`,
	).Scan(&have))
	assert.Equal(t, 1, have)
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_opencode_usage_activity_observed'`,
	).Scan(&have))
	assert.Equal(t, 1, have)
}
