package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Regression retained from the independent cold review of launch briefs.
func TestLegacyOperationMigrationRetainsInitialMessageDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE operations(
		id TEXT PRIMARY KEY, request_id TEXT NOT NULL UNIQUE, kind TEXT NOT NULL,
		principal_kind TEXT NOT NULL, principal_agent_id TEXT NOT NULL DEFAULT '',
		execution_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
		result_code TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store, err := Open(path)
	require.NoError(t, err)
	defer store.Close()

	_, err = store.db.Exec(`INSERT INTO operations(
		id, request_id, request_scope, kind, principal_kind, state,
		initial_message_digest, revision, created_at, updated_at
	) VALUES('op', 'req', 'operator', 'launch', 'operator', 'admitted', 'digest', 1, 1, 1)`)
	require.NoError(t, err)
}
