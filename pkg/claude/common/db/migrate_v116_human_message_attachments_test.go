package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV115toV116AddsHumanMessageAttachments(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v116?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (115);
		CREATE TABLE human_messages (id INTEGER PRIMARY KEY);
		PRAGMA foreign_keys = ON;
	`)
	require.NoError(t, err)
	require.NoError(t, migrateV115toV116(d))
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	require.Equal(t, 116, version)
	var tables int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='human_message_attachments'`).Scan(&tables))
	require.Equal(t, 1, tables)
}
