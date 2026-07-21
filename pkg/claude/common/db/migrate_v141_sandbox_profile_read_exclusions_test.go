package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV141AddsReadExclusionsWithCompatibleDefault(t *testing.T) {
	require.Equal(t, 141, currentVersion, "tripwire: bump this with the next migration")
	d := newV139DB(t, "migrate-v141-default")
	mustExec(t, d, `UPDATE schema_version SET version = 140`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`)
	mustExec(t, d, `INSERT INTO sandbox_profiles (name, created_at, updated_at) VALUES ('legacy', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)

	require.NoError(t, migrateV140toV141(d))
	var value string
	require.NoError(t, d.QueryRow(`SELECT read_baseline_exclusions_json FROM sandbox_profiles WHERE name = 'legacy'`).Scan(&value))
	assert.Equal(t, "[]", value)
	assert.Equal(t, 141, schemaVersion(d))
}

func TestMigrateV141IsIdempotentAndTableOptional(t *testing.T) {
	d := newV139DB(t, "migrate-v141-idempotent")
	mustExec(t, d, `UPDATE schema_version SET version = 140`)
	require.NoError(t, migrateV140toV141(d))
	mustExec(t, d, `UPDATE schema_version SET version = 140`)
	mustExec(t, d, `CREATE TABLE sandbox_profiles (id INTEGER PRIMARY KEY, read_baseline_exclusions_json TEXT NOT NULL DEFAULT '[]')`)
	require.NoError(t, migrateV140toV141(d))
}
