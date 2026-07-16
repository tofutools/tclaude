package db

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV131toV132AddsCodexTelemetryCheckpoints(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v132?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `PRAGMA foreign_keys = ON`)
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (131)`)
	mustExec(t, d, `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)

	require.NoError(t, migrateV131toV132(d))
	assert.Equal(t, 132, schemaVersion(d))
	require.NoError(t, migrateV131toV132(d), "migration must converge after a partially applied schema change")

	mustExec(t, d, `INSERT INTO sessions (id) VALUES ('codex-session')`)
	mustExec(t, d, `INSERT INTO codex_telemetry_checkpoints (session_id, data, updated_at)
		VALUES ('codex-session', '{}', '')`)
	mustExec(t, d, `DELETE FROM sessions WHERE id = 'codex-session'`)
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM codex_telemetry_checkpoints`).Scan(&count))
	assert.Zero(t, count, "session deletion cascades to its follower checkpoint")
}

func TestCodexTelemetryCheckpointRoundTrip(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, SaveSession(&SessionRow{ID: "codex-session", ConvID: "codex-conv", Status: "idle"}))
	want := json.RawMessage(`{"version":1,"offset":42}`)
	require.NoError(t, SaveCodexTelemetryCheckpoint("codex-session", want))
	got, err := LoadCodexTelemetryCheckpoint("codex-session")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.JSONEq(t, string(want), string(got.Data))
	assert.Zero(t, got.FailureCount)

	failures, err := IncrementCodexTelemetryCheckpointFailures("codex-session")
	require.NoError(t, err)
	assert.Equal(t, 1, failures)
	got, err = LoadCodexTelemetryCheckpoint("codex-session")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, got.FailureCount)

	// A successful replacement checkpoint clears prior processing failures.
	require.NoError(t, SaveCodexTelemetryCheckpoint("codex-session", want))
	got, err = LoadCodexTelemetryCheckpoint("codex-session")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Zero(t, got.FailureCount)
	require.NoError(t, DeleteCodexTelemetryCheckpoint("codex-session"))
	got, err = LoadCodexTelemetryCheckpoint("codex-session")
	require.NoError(t, err)
	assert.Nil(t, got)
}
