package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV49toV50_AddsEngineMode seeds a bare v49 DB (with the v49 workgraph
// tables), runs the v50 migration, and asserts workgraph_instances.engine_mode
// lands with the right default and is writable. Plain ALTER TABLE ADD COLUMN
// migration — a pre-existing row reads back the 'system' default (JOH-15 B1).
func TestMigrateV49toV50_AddsEngineMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v49.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal v49 workgraph_instances table with one pre-existing row (the
	// columns the v48→v49 migration created, sans engine_mode).
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (49);
		CREATE TABLE workgraph_instances (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			template_ref  TEXT NOT NULL,
			template_name TEXT NOT NULL,
			title         TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT 'running',
			mermaid       TEXT NOT NULL DEFAULT '',
			params        TEXT NOT NULL DEFAULT '{}',
			vars          TEXT NOT NULL DEFAULT '{}',
			group_id      INTEGER NOT NULL DEFAULT 0,
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL,
			completed_at  TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO workgraph_instances (template_ref, template_name, created_at, updated_at)
			VALUES ('example:demo', 'demo', '2026-05-28T00:00:00Z', '2026-05-28T00:00:00Z');
	`)
	require.NoError(t, err, "seed v49 schema")

	require.NoError(t, migrateV49toV50(d), "migrateV49toV50")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 50, ver, "schema_version after migration")

	// The pre-existing row defaults to the system engine (back-compat).
	var mode string
	require.NoError(t, d.QueryRow(`SELECT engine_mode FROM workgraph_instances WHERE template_name = 'demo'`).Scan(&mode))
	assert.Equal(t, "system", mode, "pre-existing row defaults engine_mode to 'system'")

	// The column is writable (an agent-engine instance).
	_, err = d.Exec(`UPDATE workgraph_instances SET engine_mode = 'agent' WHERE template_name = 'demo'`)
	require.NoError(t, err, "write engine_mode")
	require.NoError(t, d.QueryRow(`SELECT engine_mode FROM workgraph_instances WHERE template_name = 'demo'`).Scan(&mode))
	assert.Equal(t, "agent", mode, "engine_mode round-trips")
}

// TestMigrateV49toV50_FreshSchemaHasEngineMode builds a fresh DB through the full
// migrate() chain and confirms engine_mode round-trips through the Insert/Get
// accessors — an explicit 'agent' is stored, and an omitted value defaults to
// 'system'. Carries the literal currentVersion pin (the tripwire) — the next
// migration's author moves it forward into their own v51 test.
func TestMigrateV49toV50_FreshSchemaHasEngineMode(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 50, currentVersion, "currentVersion is 50")

	// An explicit agent-engine instance round-trips its mode.
	agentID, err := InsertWorkgraphInstance(&WorkgraphInstance{
		TemplateRef: "user:agentwf", TemplateName: "agentwf", EngineMode: "agent",
	})
	require.NoError(t, err, "InsertWorkgraphInstance(agent) on a fresh schema")
	got, err := GetWorkgraphInstance(agentID)
	require.NoError(t, err, "GetWorkgraphInstance")
	require.NotNil(t, got)
	assert.Equal(t, "agent", got.EngineMode, "explicit engine_mode round-trips")

	// An omitted engine_mode defaults to 'system' at the DB layer.
	sysID, err := InsertWorkgraphInstance(&WorkgraphInstance{
		TemplateRef: "user:syswf", TemplateName: "syswf",
	})
	require.NoError(t, err, "InsertWorkgraphInstance(default)")
	gotSys, err := GetWorkgraphInstance(sysID)
	require.NoError(t, err, "GetWorkgraphInstance")
	require.NotNil(t, gotSys)
	assert.Equal(t, "system", gotSys.EngineMode, "omitted engine_mode defaults to 'system'")
}
