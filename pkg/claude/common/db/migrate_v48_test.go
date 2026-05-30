package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV47toV48_AddsWorkflowTables seeds a bare v47 DB, runs the
// v48 migration, and asserts the three workflow tables land and are
// writable. Plain CREATE TABLE migration — no pre-existing-row concern.
// foreign_keys is enabled on the raw handle so the CASCADE the schema
// declares is exercised exactly as production runs it.
func TestMigrateV47toV48_AddsWorkflowTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v47.sqlite")
	d, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (47);
	`)
	require.NoError(t, err, "seed v47 schema")

	require.NoError(t, migrateV47toV48(d), "migrateV47toV48")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 48, ver, "schema_version after migration")

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

// TestMigrateV47toV48_FreshSchemaHasWorkflowTables builds a fresh DB
// through the full migrate() chain and confirms the workflow accessors
// work end to end. Carries the literal currentVersion pin — a tripwire
// the next migration's author moves forward into their own v49 test.
func TestMigrateV47toV48_FreshSchemaHasWorkflowTables(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 48, currentVersion, "currentVersion is 48")

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

// TestMigrateV46throughV48_ChainAppliesInOrder pins the ordering of the
// two adjacent migrations that collided when main (sessions.model, v47)
// was merged into the workflows branch (workflow tables, renumbered to
// v48). It seeds a DB at v46 — the state of a real user DB just before
// this branch's two steps — and runs them in production order:
//
//	v46 → migrateV46toV47 (main, ALTER sessions ADD model)
//	    → migrateV47toV48 (ours, CREATE workflow_* tables)
//
// and asserts each step lands its own artifact and bumps schema_version,
// and that the two coexist at the end. The two migrations touch disjoint
// tables, so the real failure this guards against is a botched merge
// resolution — a swapped gate, a duplicated step, or a version stamp
// that skips a number — corrupting the upgrade path for already-deployed
// v47 DBs. Distinct from the per-migration tests above (each exercises
// one step in isolation) by walking the ordered pair end to end.
func TestMigrateV46throughV48_ChainAppliesInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v46.sqlite")
	d, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal v46 DB: schema_version + a sessions table (so the v47
	// ALTER ... ADD COLUMN model has a table to alter), with one row.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (46);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'idle');
		INSERT INTO sessions (id, status) VALUES ('s1', 'idle');
	`)
	require.NoError(t, err, "seed v46 schema")

	// Step 1: v46 → v47 (main's sessions.model).
	require.NoError(t, migrateV46toV47(d), "migrateV46toV47")
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, 47, ver, "schema_version after the v47 step")
	_, err = d.Exec(`UPDATE sessions SET model = 'Opus 4.8' WHERE id = 's1'`)
	require.NoError(t, err, "sessions.model exists after the v47 step")

	// Step 2: v47 → v48 (our workflow tables), applied AFTER v47.
	require.NoError(t, migrateV47toV48(d), "migrateV47toV48")
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, 48, ver, "schema_version after the v48 step")

	// Both migrations' artifacts coexist on the upgraded DB.
	_, err = d.Exec(`INSERT INTO workflow_instances
		(template_ref, template_name, created_at, updated_at)
		VALUES ('x:y', 'y', '2026-05-28T00:00:00Z', '2026-05-28T00:00:00Z')`)
	require.NoError(t, err, "workflow_instances usable after the chain")
	var model string
	require.NoError(t, d.QueryRow(`SELECT model FROM sessions WHERE id = 's1'`).Scan(&model))
	assert.Equal(t, "Opus 4.8", model, "sessions.model survives the v48 step")
}
