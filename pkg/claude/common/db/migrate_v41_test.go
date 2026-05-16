package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV40toV41_AddsTargetKind seeds a v40 agent_cron_jobs table
// holding a conv-targeted job — a row written before target_kind
// existed — runs the v41 migration, and asserts the new column lands
// with the existing row backfilled to 'conv'. A group-kind row then
// inserts cleanly, and the CHECK constraint rejects an unknown kind.
func TestMigrateV40toV41_AddsTargetKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v40.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Seed the v40 agent_cron_jobs shape (unchanged since v13) plus one
	// conv-targeted job. This is exactly the row a pre-v41 daemon wrote.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (40);

		CREATE TABLE agent_cron_jobs (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT NOT NULL DEFAULT '',
			owner_conv       TEXT NOT NULL,
			target_conv      TEXT NOT NULL,
			group_id         INTEGER NOT NULL DEFAULT 0,
			interval_seconds INTEGER NOT NULL,
			subject          TEXT NOT NULL DEFAULT '',
			body             TEXT NOT NULL DEFAULT '',
			enabled          INTEGER NOT NULL DEFAULT 1,
			created_at       TEXT NOT NULL,
			last_run_at      TEXT NOT NULL DEFAULT '',
			last_run_status  TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO agent_cron_jobs
			(name, owner_conv, target_conv, group_id, interval_seconds,
			 subject, body, enabled, created_at)
			VALUES ('legacy', 'owner-conv', 'target-conv', 0, 600,
			        'subj', 'body', 1, '2026-05-16T00:00:00Z');
	`)
	require.NoError(t, err, "seed v40 schema")

	require.NoError(t, migrateV40toV41(d), "migrateV40toV41")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 41, ver, "schema_version after migration")

	// The pre-existing conv-targeted job survives and defaults to 'conv'
	// — every column it carried is untouched.
	var kind, targetConv, body string
	require.NoError(t, d.QueryRow(
		`SELECT target_kind, target_conv, body FROM agent_cron_jobs WHERE name = 'legacy'`).
		Scan(&kind, &targetConv, &body))
	assert.Equal(t, "conv", kind, "existing rows backfill to target_kind=conv")
	assert.Equal(t, "target-conv", targetConv, "existing target_conv preserved")
	assert.Equal(t, "body", body, "existing body preserved")

	// A group-kind job inserts cleanly post-migration.
	_, err = d.Exec(`
		INSERT INTO agent_cron_jobs
			(name, owner_conv, target_kind, target_conv, group_id,
			 interval_seconds, body, enabled, created_at)
			VALUES ('multicast', 'owner-conv', 'group', '', 7, 600,
			        'ping', 1, '2026-05-16T00:00:00Z')`)
	require.NoError(t, err, "insert group-kind job")

	// The CHECK constraint rejects a target_kind outside {conv, group},
	// so a stray write cannot leave a job the scheduler cannot classify.
	_, err = d.Exec(`
		INSERT INTO agent_cron_jobs
			(name, owner_conv, target_kind, target_conv, group_id,
			 interval_seconds, body, enabled, created_at)
			VALUES ('bad', 'o', 'banana', '', 0, 600, 'x', 1, '2026-05-16T00:00:00Z')`)
	require.Error(t, err, "target_kind CHECK rejects an unknown value")

	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agent_cron_jobs`).Scan(&n))
	assert.Equal(t, 2, n, "legacy + multicast rows present; the bad-kind row was rejected")
}

// TestMigrateV40toV41_FreshSchemaHasTargetKind builds a fresh DB through
// the full migrate() chain (via Open) and confirms agent_cron_jobs has
// target_kind defaulting to 'conv' — pins that the v41 block is wired
// into the dispatcher, not just defined.
func TestMigrateV40toV41_FreshSchemaHasTargetKind(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	// An insert that omits target_kind lands as 'conv' — the DEFAULT
	// holds on a fresh schema built through the full chain.
	_, err = d.Exec(`
		INSERT INTO agent_cron_jobs
			(name, owner_conv, target_conv, interval_seconds, body, enabled, created_at)
			VALUES ('c', 'o', 't', 600, 'b', 1, '2026-05-16T00:00:00Z')`)
	require.NoError(t, err, "insert without target_kind")
	var kind string
	require.NoError(t, d.QueryRow(
		`SELECT target_kind FROM agent_cron_jobs WHERE name = 'c'`).Scan(&kind))
	assert.Equal(t, "conv", kind, "target_kind defaults to conv")
}
