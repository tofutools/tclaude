package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrateV228ToV229KeychainDefaultsAndPreservesOptOut(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v228.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (228)`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (id INTEGER PRIMARY KEY) STRICT`)
	mustExec(t, d, `INSERT INTO sandbox_profiles VALUES (1)`)
	require.NoError(t, migrateV228toV229(d))
	var disabled bool
	require.NoError(t, d.QueryRow(`SELECT darwin_disable_keychain_write FROM sandbox_profiles WHERE id = 1`).Scan(&disabled))
	require.False(t, disabled, "existing profiles retain the automatic Keychain grant")
	mustExec(t, d, `UPDATE sandbox_profiles SET darwin_disable_keychain_write = 1`)
	require.NoError(t, migrateV228toV229(d))
	require.NoError(t, d.QueryRow(`SELECT darwin_disable_keychain_write FROM sandbox_profiles WHERE id = 1`).Scan(&disabled))
	require.True(t, disabled)
	require.Equal(t, 229, schemaVersion(d))
}
