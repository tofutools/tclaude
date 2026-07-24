package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV146AddsProcessRuntimeStoreAdditively(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v146-process-runs?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (145)`)
	mustExec(t, d, `CREATE TABLE preserve_me (value TEXT NOT NULL)`)
	mustExec(t, d, `INSERT INTO preserve_me VALUES ('yes')`)

	require.NoError(t, migrateV145toV146(d))
	assert.Equal(t, 146, schemaVersion(d))
	require.NoError(t, migrateV145toV146(d), "repeated migration converges")

	for _, table := range []string{"process_runs", "process_run_events", "preserve_me"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
	var preserved string
	require.NoError(t, d.QueryRow(`SELECT value FROM preserve_me`).Scan(&preserved))
	assert.Equal(t, "yes", preserved)

	var indexSQL string
	require.NoError(t, d.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_process_runs_active'`).Scan(&indexSQL))
	assert.Contains(t, indexSQL, "status NOT IN")
}

func TestMigrateV146RunAndEventConstraints(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v146-constraints?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (145)`)
	require.NoError(t, migrateV145toV146(d))

	insertRun := `INSERT INTO process_runs
		(id, template_ref, template_snapshot_json, params_json, status, state_version,
		 checkpoint_json, created_at, updated_at)
		VALUES ('run_one', 't@sha256:x', '{}', '{}', 'running', 1, '{}', 'now', 'now')`
	mustExec(t, d, insertRun)
	_, err = d.Exec(insertRun)
	assert.Error(t, err, "run ids are unique")
	_, err = d.Exec(`INSERT INTO process_runs
		(id, template_ref, template_snapshot_json, params_json, status, state_version,
		 checkpoint_json, created_at, updated_at)
		VALUES (NULL, 't@sha256:x', '{}', '{}', 'running', 1, '{}', 'now', 'now')`)
	assert.Error(t, err, "the textual primary key must reject NULL")
	_, err = d.Exec(`INSERT INTO process_runs
		(id, template_ref, template_snapshot_json, params_json, status, state_version,
		 checkpoint_json, created_at, updated_at)
		VALUES ('run_oversized', 't@sha256:x', '{}', '{}', 'running', 1, ?, 'now', 'now')`,
		strings.Repeat("é", MaxProcessRunCheckpointBytes/2+1))
	assert.Error(t, err, "checkpoint constraints count bytes and reject oversized values")

	mustExec(t, d, `INSERT INTO process_run_events
		(run_id, sequence, occurred_at, kind, payload_json) VALUES ('run_one', 1, 'now', 'created', '{}')`)
	_, err = d.Exec(`INSERT INTO process_run_events
		(run_id, sequence, occurred_at, kind, payload_json) VALUES ('run_one', 1, 'later', 'duplicate', '{}')`)
	assert.Error(t, err, "event sequences are unique within a run")

	mustExec(t, d, `DELETE FROM process_runs WHERE id = 'run_one'`)
	var events int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM process_run_events`).Scan(&events))
	assert.Zero(t, events, "run deletion cascades to evidence")
}

func TestFreshSchemaHasProcessRuntimeStore(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	for _, table := range []string{"process_runs", "process_run_events"} {
		var count int
		require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count))
		assert.Equal(t, 1, count, table)
	}
}

func TestMigrateV152IsTheCurrentHead(t *testing.T) {
	require.Equal(t, 152, currentVersion, "tripwire: bump this with the next migration")
}
