package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV132toV133AddsCostWalkIndex(t *testing.T) {
	require.Equal(t, 133, currentVersion, "tripwire: bump this with the next migration")
	d, err := sql.Open("sqlite", "file:migrate-v133?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (132)`)
	mustExec(t, d, `CREATE TABLE session_cost_daily (
		session_id TEXT NOT NULL,
		day TEXT NOT NULL,
		conv_id TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (session_id, day)
	)`)
	mustExec(t, d, `INSERT INTO session_cost_daily(session_id, day, conv_id, updated_at) VALUES
		('spwn-b', '2026-07-02', 'conv-a', '2026-07-02T12:00:00Z'),
		('conv-a', '2026-07-02', 'conv-a', '2026-07-02T13:00:00Z'),
		('orphan', '2026-07-01', '', '2026-07-01T12:00:00Z')`)

	require.NoError(t, migrateV132toV133(d))
	assert.Equal(t, 133, schemaVersion(d))
	require.NoError(t, migrateV132toV133(d), "migration converges after a partially applied schema change")

	planRows, err := d.Query(`EXPLAIN QUERY PLAN
		SELECT session_id, day, conv_id, updated_at
		FROM session_cost_daily
		ORDER BY COALESCE(NULLIF(conv_id, ''), session_id), day, updated_at, session_id`)
	require.NoError(t, err)
	defer func() { _ = planRows.Close() }()
	var plan strings.Builder
	for planRows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, planRows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	require.NoError(t, planRows.Err())
	assert.Contains(t, plan.String(), "USING INDEX idx_session_cost_daily_walk")
	assert.NotContains(t, plan.String(), "USE TEMP B-TREE")
}

func TestFreshSchemaHasCostWalkIndex(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_session_cost_daily_walk'`).Scan(&count))
	assert.Equal(t, 1, count)
}
