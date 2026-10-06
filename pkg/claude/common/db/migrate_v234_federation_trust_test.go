package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestMigrateV233ToV234TrustDefaultsRestricted(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(testutil.CanonicalTempDir(t), "v233.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES(233);
 CREATE TABLE federation_peers(instance_id TEXT PRIMARY KEY, pubkey BLOB NOT NULL, label TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', trusted_at INTEGER NOT NULL) STRICT;
 INSERT INTO federation_peers VALUES('old', x'01', 'bob', 'Bob', 42);`)
	require.NoError(t, migrateV233toV234(d))
	var level, label string
	require.NoError(t, d.QueryRow(`SELECT trust_level, label FROM federation_peers WHERE instance_id='old'`).Scan(&level, &label))
	require.Equal(t, FederationTrustRestricted, level)
	require.Equal(t, "bob", label)
	require.Equal(t, 234, schemaVersion(d))
	mustExec(t, d, `INSERT INTO federation_peers(instance_id,pubkey,trusted_at) VALUES('new',x'02',43)`)
	require.NoError(t, d.QueryRow(`SELECT trust_level FROM federation_peers WHERE instance_id='new'`).Scan(&level))
	require.Equal(t, FederationTrustRestricted, level)
	_, err = d.Exec(`UPDATE federation_peers SET trust_level='unknown'`)
	require.Error(t, err)
}
