package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV152toV153OpenCodeStepRemovals(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'opencode_usage_step_removals'`,
	).Scan(&have))
	assert.Equal(t, 1, have)
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_opencode_usage_step_removals_removed'`,
	).Scan(&have))
	assert.Equal(t, 1, have)
}
