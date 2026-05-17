package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV44toV45_AddsConvBranchHistory seeds a bare v44 DB, runs
// the v45 migration, and asserts the conv_branch_history table lands
// and is writable. The migration is a plain CREATE TABLE, so there is
// no pre-existing-row concern — the table simply must exist afterwards.
func TestMigrateV44toV45_AddsConvBranchHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v44.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (44);
	`)
	require.NoError(t, err, "seed v44 schema")

	require.NoError(t, migrateV44toV45(d), "migrateV44toV45")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 45, ver, "schema_version after migration")

	// The table exists and accepts a row, including a PR-absent one.
	_, err = d.Exec(`INSERT INTO conv_branch_history
		(conv_id, branch, repo_dir, source, first_seen, last_seen)
		VALUES ('c1', 'feature-x', '/repo', 'scan',
		        '2026-05-17T00:00:00Z', '2026-05-17T01:00:00Z')`)
	require.NoError(t, err, "insert PR-absent row into conv_branch_history")

	var prNumber int
	var prURL, prState string
	require.NoError(t, d.QueryRow(
		`SELECT pr_number, pr_url, pr_state FROM conv_branch_history
		 WHERE conv_id = 'c1' AND branch = 'feature-x'`).
		Scan(&prNumber, &prURL, &prState))
	assert.Zero(t, prNumber, "a PR-absent row defaults pr_number to 0")
	assert.Empty(t, prURL, "a PR-absent row defaults pr_url to ''")
	assert.Empty(t, prState, "a PR-absent row defaults pr_state to ''")

	// (conv_id, branch) is the primary key — the same branch twice in
	// one conv is a constraint violation, not a duplicate row.
	_, err = d.Exec(`INSERT INTO conv_branch_history (conv_id, branch)
		VALUES ('c1', 'feature-x')`)
	require.Error(t, err, "(conv_id, branch) is unique")
}

// TestMigrateV44toV45_FreshSchemaHasConvBranchHistory builds a fresh DB
// through the full migrate() chain and confirms conv_branch_history
// exists and the table ops work end to end. It also carries the literal
// currentVersion pin — a tripwire that the next migration's author
// moves forward into their own v46 test.
func TestMigrateV44toV45_FreshSchemaHasConvBranchHistory(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
	require.Equal(t, 45, currentVersion, "currentVersion is 45")

	// The table built through the migration chain is usable.
	require.NoError(t, AppendConvBranchHistoryHook("c1", "feature-x", "/repo"))
	rows, err := ListConvBranchHistory("c1")
	require.NoError(t, err, "ListConvBranchHistory on a fresh schema")
	require.Len(t, rows, 1)
	assert.Equal(t, "feature-x", rows[0].Branch)
}
