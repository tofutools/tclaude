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
	mustExec(t, d, `ALTER TABLE copilot_usage_snapshots DROP COLUMN fold_version`)
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
	var notNull int
	var defaultValue any
	require.NoError(t, d.QueryRow(`SELECT "notnull", dflt_value
		FROM pragma_table_info('copilot_usage_snapshots') WHERE name = 'fold_version'`).
		Scan(&notNull, &defaultValue))
	assert.Equal(t, 1, notNull)
	assert.Nil(t, defaultValue, "no default makes a still-running v194 writer fail closed")

	// This is v194's essential INSERT...ON CONFLICT shape after a new daemon
	// has migrated the database. Omitting fold_version must fail when recreating
	// the deleted row, or the old daemon could make a bad cursor look current.
	_, err = d.Exec(`INSERT INTO copilot_usage_snapshots
		(session_id, conv_id, last_event_id, observed_at)
		VALUES ('s-copilot', 'conv-1', 99, 1)
		ON CONFLICT(session_id) DO UPDATE SET last_event_id = excluded.last_event_id`)
	require.ErrorContains(t, err, "fold_version")

	// It must also fail against a trusted row written by v195. SQLite checks
	// the omitted NOT NULL column before entering the conflict-update arm, so
	// the old writer cannot overwrite the new fold while leaving its marker at
	// version 1.
	trusted := sampleCopilotUsageSnapshot("s-copilot", "conv-1")
	trusted.LastEventID = 2
	saved, err = SaveCopilotUsageSnapshot(trusted, createdAt)
	require.NoError(t, err)
	require.True(t, saved)
	_, err = d.Exec(`INSERT INTO copilot_usage_snapshots
		(session_id, conv_id, last_event_id, observed_at)
		VALUES ('s-copilot', 'conv-1', 99, 1)
		ON CONFLICT(session_id) DO UPDATE SET last_event_id = excluded.last_event_id`)
	require.ErrorContains(t, err, "fold_version")
	unchanged, err := LoadCopilotUsageSnapshot("s-copilot")
	require.NoError(t, err)
	require.NotNil(t, unchanged)
	assert.Equal(t, int64(2), unchanged.LastEventID)
	assert.Equal(t, CopilotUsageFoldVersion, unchanged.FoldVersion)
	assert.Equal(t, 195, schemaVersion(d))
	require.NoError(t, migrateV194toV195(d), "a partially applied reset converges")
}
