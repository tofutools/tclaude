package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV95toV96_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. The literal currentVersion
// tripwire has moved forward to the v97 test (migrate_v97_session_ask_timeout_test.go).
func TestMigrateV95toV96_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
}

// TestMigrateV95toV96_AddsColumn drives the real v95→v96 ALTER over a v95-pinned
// DB: it asserts spawn_profiles.ask_user_question_timeout appears, that a
// pre-existing profile reads back as "" (no override), that the version
// advances, and that a re-run is a clean no-op.
func TestMigrateV95toV96_AddsColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v95 and drop the new column so we re-add it from a true v95
	// shape (the fresh chain already ran v96). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE spawn_profiles DROP COLUMN ask_user_question_timeout`)
	mustExec(t, d, `UPDATE schema_version SET version = 95`)

	// A pre-existing profile (without the new column) must survive the ALTER and
	// read back with the default.
	mustExec(t, d, `INSERT INTO spawn_profiles (name, created_at, updated_at)
		VALUES ('legacy', '2026-07-04T00:00:00Z', '2026-07-04T00:00:00Z')`)

	require.NoError(t, migrateV95toV96(d), "v95→v96")

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'ask_user_question_timeout'`).Scan(&n))
	assert.Equal(t, 1, n, "spawn_profiles.ask_user_question_timeout added")

	var v string
	require.NoError(t, d.QueryRow(
		`SELECT ask_user_question_timeout FROM spawn_profiles WHERE name = 'legacy'`).Scan(&v))
	assert.Equal(t, "", v, "existing profile defaults to no override")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 96, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guard skips the duplicate ADD COLUMN).
	require.NoError(t, migrateV95toV96(d), "v95→v96 re-run is a clean no-op")
}
