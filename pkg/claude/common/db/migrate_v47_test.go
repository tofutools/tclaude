package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV46toV47_AddsWorkflowTables seeds a bare v46 DB, runs the
// v47 migration, and asserts the three workflow tables land and are
// writable. Plain CREATE TABLE migration — no pre-existing-row concern.
// foreign_keys is enabled on the raw handle so the CASCADE the schema
// declares is exercised exactly as production runs it.
func TestMigrateV46toV47_AddsWorkflowTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v46.sqlite")
	d, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (46);
	`)
	require.NoError(t, err, "seed v46 schema")

	require.NoError(t, migrateV46toV47(d), "migrateV46toV47")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 47, ver, "schema_version after migration")

	// Instance row with all-default JSON/status columns accepted.
	res, err := d.Exec(`INSERT INTO workflow_instances
		(template_ref, template_name, created_at, updated_at)
		VALUES ('example:demo', 'demo', '2026-05-28T00:00:00Z', '2026-05-28T00:00:00Z')`)
	require.NoError(t, err, "insert instance with defaulted columns")
	instID, err := res.LastInsertId()
	require.NoError(t, err)

	var status, params, vars string
	require.NoError(t, d.QueryRow(
		`SELECT status, params, vars FROM workflow_instances WHERE id = ?`, instID).
		Scan(&status, &params, &vars))
	assert.Equal(t, "running", status, "status defaults to running")
	assert.Equal(t, "{}", params, "params defaults to {}")
	assert.Equal(t, "{}", vars, "vars defaults to {}")

	// A node + an event for that instance.
	_, err = d.Exec(`INSERT INTO workflow_nodes
		(instance_id, node_id, updated_at) VALUES (?, 'n1', '2026-05-28T00:00:00Z')`, instID)
	require.NoError(t, err, "insert node")
	_, err = d.Exec(`INSERT INTO workflow_events
		(instance_id, kind, at) VALUES (?, 'instance_created', '2026-05-28T00:00:00Z')`, instID)
	require.NoError(t, err, "insert event")

	// UNIQUE(instance_id, node_id): a second node with the same node_id fails.
	_, err = d.Exec(`INSERT INTO workflow_nodes
		(instance_id, node_id, updated_at) VALUES (?, 'n1', '2026-05-28T00:00:00Z')`, instID)
	require.Error(t, err, "(instance_id, node_id) is unique")

	// ON DELETE CASCADE: deleting the instance clears its nodes + events.
	_, err = d.Exec(`DELETE FROM workflow_instances WHERE id = ?`, instID)
	require.NoError(t, err, "delete instance")

	var nodeCount, eventCount int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM workflow_nodes WHERE instance_id = ?`, instID).Scan(&nodeCount))
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM workflow_events WHERE instance_id = ?`, instID).Scan(&eventCount))
	assert.Zero(t, nodeCount, "nodes cascade-deleted with instance")
	assert.Zero(t, eventCount, "events cascade-deleted with instance")
}

// TestMigrateV46toV47_FreshSchemaHasWorkflowTables builds a fresh DB
// through the full migrate() chain and confirms the workflow accessors
// work end to end. Carries the literal currentVersion pin — a tripwire
// the next migration's author moves forward into their own v48 test.
func TestMigrateV46toV47_FreshSchemaHasWorkflowTables(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 47, currentVersion, "currentVersion is 47")

	id, err := InsertWorkflowInstance(&WorkflowInstance{
		TemplateRef:  "user:release",
		TemplateName: "release",
		Title:        "Release 1.0",
	})
	require.NoError(t, err, "InsertWorkflowInstance on a fresh schema")
	got, err := GetWorkflowInstance(id)
	require.NoError(t, err, "GetWorkflowInstance")
	require.NotNil(t, got)
	assert.Equal(t, "release", got.TemplateName)
	assert.Equal(t, WorkflowStatusRunning, got.Status)
}
