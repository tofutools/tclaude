package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateRefusesDatabaseFromNewerBinary(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "newer.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, createSchema(d))
	require.NoError(t, setSchemaVersion(d, currentVersion+1))

	err = migrate(d)
	require.Error(t, err, "newer-schema refusal arm must execute")
	assert.ErrorContains(t, err, "database was created by a newer tclaude; refusing to open")
	assert.ErrorContains(t, err, fmt.Sprintf("database schema version %d", currentVersion+1))
	assert.ErrorContains(t, err, fmt.Sprintf("this binary supports %d", currentVersion))
}

func TestMigrateCurrentDatabaseRemainsBenignNoOp(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "current.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, createSchema(d))
	require.NoError(t, setSchemaVersion(d, currentVersion))

	require.NoError(t, migrate(d))
	var got int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&got))
	assert.Equal(t, currentVersion, got)
}

func setSchemaVersion(d *sql.DB, version int) error {
	_, err := d.Exec(`UPDATE schema_version SET version = ?`, version)
	return err
}
