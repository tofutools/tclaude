package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV45toV46_AddsAgentWorkspace seeds a bare v45 DB, runs the
// v46 migration, and asserts the agent_workspace table lands and is
// writable. Plain CREATE TABLE migration — no pre-existing-row concern.
func TestMigrateV45toV46_AddsAgentWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v45.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (45);
	`)
	require.NoError(t, err, "seed v45 schema")

	require.NoError(t, migrateV45toV46(d), "migrateV45toV46")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 46, ver, "schema_version after migration")

	// Table exists and accepts a row, including a PR-absent one.
	_, err = d.Exec(`INSERT INTO agent_workspace
		(conv_id, cwd, branch, repo_url, default_branch, updated_at)
		VALUES ('c1', '/repo', 'feature-x', 'https://github.com/o/r', 'main',
		        '2026-05-17T00:00:00Z')`)
	require.NoError(t, err, "insert PR-absent row into agent_workspace")

	var prNumber int
	var prURL, prState string
	require.NoError(t, d.QueryRow(
		`SELECT pr_number, pr_url, pr_state FROM agent_workspace WHERE conv_id = 'c1'`).
		Scan(&prNumber, &prURL, &prState))
	assert.Zero(t, prNumber, "a PR-absent row defaults pr_number to 0")
	assert.Empty(t, prURL, "a PR-absent row defaults pr_url to ''")
	assert.Empty(t, prState, "a PR-absent row defaults pr_state to ''")

	// conv_id is the primary key — a second insert for the same conv
	// is a constraint violation, not a duplicate row.
	_, err = d.Exec(`INSERT INTO agent_workspace (conv_id, cwd) VALUES ('c1', '/x')`)
	require.Error(t, err, "conv_id is unique")
}

// TestMigrateV45toV46_FreshSchemaHasAgentWorkspace builds a fresh DB
// through the full migrate() chain and confirms agent_workspace exists
// and the accessors work end to end. The literal currentVersion pin
// (the "next migration moves this forward" tripwire) now lives in
// migrate_v47_test.go.
func TestMigrateV45toV46_FreshSchemaHasAgentWorkspace(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	require.NoError(t, UpsertAgentWorkspace(AgentWorkspace{
		ConvID: "c1", Cwd: "/repo", Branch: "feature-x",
		RepoURL: "https://github.com/o/r", DefaultBranch: "main",
	}))
	got, err := GetAgentWorkspace("c1")
	require.NoError(t, err, "GetAgentWorkspace on a fresh schema")
	assert.Equal(t, "feature-x", got.Branch)
	assert.Equal(t, "https://github.com/o/r", got.RepoURL)
}
