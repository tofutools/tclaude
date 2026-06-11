package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV49toV50_AddsCostUSDColumn seeds a bare v49 DB, runs the
// v50 migration, and asserts the sessions.cost_usd column lands with
// the right default and is writable. Plain ALTER TABLE ADD COLUMN
// migration — a pre-existing row reads back the 0 default ("no cost
// data").
func TestMigrateV49toV50_AddsCostUSDColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v49.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// A minimal v49 sessions table with one pre-existing row.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (49);
		CREATE TABLE sessions (id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'idle');
		INSERT INTO sessions (id, status) VALUES ('pre-existing', 'idle');
	`)
	require.NoError(t, err, "seed v49 schema")

	require.NoError(t, migrateV49toV50(d), "migrateV49toV50")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 50, ver, "schema_version after migration")

	// The pre-existing row defaults to "no cost data".
	var cost float64
	require.NoError(t, d.QueryRow(`SELECT cost_usd FROM sessions WHERE id = 'pre-existing'`).Scan(&cost))
	assert.Equal(t, 0.0, cost, "pre-existing row defaults cost_usd to 0")

	// The column is writable.
	_, err = d.Exec(`UPDATE sessions SET cost_usd = 1.23 WHERE id = 'pre-existing'`)
	require.NoError(t, err, "write cost_usd")
	require.NoError(t, d.QueryRow(`SELECT cost_usd FROM sessions WHERE id = 'pre-existing'`).Scan(&cost))
	assert.Equal(t, 1.23, cost, "cost_usd round-trips")
}

// TestMigrateV49toV50_FreshSchemaHasCostUSDColumn builds a fresh DB
// through the full migrate() chain and confirms sessions.cost_usd
// exists and the UpdateSessionCost / GetContextSnapshot accessors work
// end to end — including the <=0 no-op guard that keeps empty renders
// and subscription sessions from ever blanking a recorded cost.
// Carries the literal currentVersion pin — a tripwire the next
// migration's author moves forward into their own v51 test.
func TestMigrateV49toV50_FreshSchemaHasCostUSDColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 50, currentVersion, "currentVersion is 50")

	require.NoError(t, SaveSession(&SessionRow{ID: "s1", Status: "idle"}), "SaveSession")

	// Statusbar write path: nonzero cost lands on the row.
	require.NoError(t, UpdateSessionCost("s1", 0.42), "UpdateSessionCost on a fresh schema")
	snap, err := GetContextSnapshot("s1")
	require.NoError(t, err, "GetContextSnapshot")
	assert.Equal(t, 0.42, snap.CostUSD, "cost_usd round-trips via the snapshot read")

	// A zero (empty render) or negative write is a no-op — the recorded
	// value survives.
	require.NoError(t, UpdateSessionCost("s1", 0), "UpdateSessionCost(0) no-ops")
	require.NoError(t, UpdateSessionCost("s1", -1), "UpdateSessionCost(-1) no-ops")
	snap, err = GetContextSnapshot("s1")
	require.NoError(t, err, "GetContextSnapshot after no-op writes")
	assert.Equal(t, 0.42, snap.CostUSD, "zero/negative writes never blank a recorded cost")

	// SaveSession's UPSERT must leave the out-of-band column alone — the
	// same hazard the context-snapshot columns guard against (a hook tick
	// between statusline renders must not wipe the cost).
	require.NoError(t, SaveSession(&SessionRow{ID: "s1", Status: "working"}), "SaveSession upsert")
	snap, err = GetContextSnapshot("s1")
	require.NoError(t, err, "GetContextSnapshot after upsert")
	assert.Equal(t, 0.42, snap.CostUSD, "SaveSession upsert preserves cost_usd")
}
