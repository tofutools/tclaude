package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV43toV44_AddsHumanMessages seeds a bare v43 DB, runs the
// v44 migration, and asserts the human_messages table lands and is
// writable. The migration is a plain CREATE TABLE, so there is no
// pre-existing-row concern — the table simply must exist afterwards.
func TestMigrateV43toV44_AddsHumanMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v43.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (43);
	`)
	require.NoError(t, err, "seed v43 schema")

	require.NoError(t, migrateV43toV44(d), "migrateV43toV44")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 44, ver, "schema_version after migration")

	// The table exists and accepts a row.
	_, err = d.Exec(`INSERT INTO human_messages
		(from_conv, from_title, group_name, subject, body, created_at, read_at)
		VALUES ('c1', 'tclaude-PO', 'dev', 'subj', 'body', '2026-05-16T00:00:00Z', '')`)
	require.NoError(t, err, "insert into human_messages")

	var body, readAt string
	require.NoError(t, d.QueryRow(
		`SELECT body, read_at FROM human_messages WHERE from_conv = 'c1'`).Scan(&body, &readAt))
	assert.Equal(t, "body", body)
	assert.Empty(t, readAt, "a fresh row is unread (read_at empty)")
}

// TestMigrateV43toV44_FreshSchemaHasHumanMessages builds a fresh DB
// through the full migrate() chain and confirms human_messages exists
// and is wired into the dispatcher. The literal currentVersion pin has
// moved on to the v45 test.
func TestMigrateV43toV44_FreshSchemaHasHumanMessages(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	// The full-width table built through createSchema is usable.
	id, err := InsertHumanMessage(&HumanMessage{FromConv: "c1", Body: "hello"})
	require.NoError(t, err, "InsertHumanMessage on a fresh schema")
	assert.Positive(t, id)
}
