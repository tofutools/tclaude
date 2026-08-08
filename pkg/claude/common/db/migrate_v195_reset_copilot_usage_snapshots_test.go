package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV194toV195ResetsDerivedCopilotUsageFold(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	createdAt := saveCopilotUsageSession(t, "s-copilot", "conv-1")
	saved, err := SaveCopilotUsageSnapshot(
		sampleCopilotUsageSnapshot("s-copilot", "conv-1"), createdAt)
	require.NoError(t, err)
	require.True(t, saved)
	mustExec(t, d, `UPDATE schema_version SET version = 194`)

	require.NoError(t, migrateV194toV195(d))
	got, err := LoadCopilotUsageSnapshot("s-copilot")
	require.NoError(t, err)
	assert.Nil(t, got, "the old cursor must not keep already-misfolded calls hidden")
	rows, err := ListSessions()
	require.NoError(t, err)
	require.Len(t, rows, 1, "the repair clears only the derived side table")
	assert.Equal(t, "s-copilot", rows[0].ID)
	assert.Equal(t, "conv-1", rows[0].ConvID)
	assert.Equal(t, 195, schemaVersion(d))
	require.NoError(t, migrateV194toV195(d), "a partially applied reset converges")
}
