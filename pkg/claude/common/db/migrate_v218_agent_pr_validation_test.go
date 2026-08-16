package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV217toV218QuarantinesExistingPresentedPRs(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v217.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (217)`)
	mustExec(t, d, `CREATE TABLE agent_prs (id INTEGER PRIMARY KEY, agent_id TEXT NOT NULL, pr_url TEXT NOT NULL, summary TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO agent_prs VALUES (1, 'agt_old', 'https://github.com/acme/secret/pull/1', '', 'open', 1, 1)`)

	require.NoError(t, migrateV217toV218(d))
	assert.Equal(t, 218, schemaVersion(d))
	var proof string
	require.NoError(t, d.QueryRow(`SELECT validated_repo_root FROM agent_prs WHERE id = 1`).Scan(&proof))
	assert.Empty(t, proof, "pre-validation rows stay untrusted")
	require.NoError(t, migrateV217toV218(d), "partially applied migration converges")
}
