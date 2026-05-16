package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV34toV35_DropsAliasColumn builds a v34-shape
// agent_group_members table (with the legacy alias column), seeds
// rows, runs the v35 migration, and asserts the alias column is gone
// while every member row survives with its other fields and the
// (group_id, conv_id) primary key intact.
func TestMigrateV34toV35_DropsAliasColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v34.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Minimal pre-v35 schema: schema_version + agent_groups +
	// agent_group_members WITH the legacy alias column, plus seeded
	// rows that exercise non-empty AND empty alias values.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (34);
		CREATE TABLE agent_groups (
			id   INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		);
		CREATE TABLE agent_group_members (
			group_id  INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			conv_id   TEXT NOT NULL,
			alias     TEXT NOT NULL DEFAULT '',
			role      TEXT NOT NULL DEFAULT '',
			descr     TEXT NOT NULL DEFAULT '',
			joined_at TEXT NOT NULL,
			PRIMARY KEY (group_id, conv_id)
		);
		CREATE INDEX idx_agent_group_members_conv ON agent_group_members(conv_id);
		INSERT INTO agent_groups (id, name) VALUES (1, 'alpha'), (2, 'beta');
		INSERT INTO agent_group_members (group_id, conv_id, alias, role, descr, joined_at) VALUES
			(1, 'conv-a', 'planner',  'lead',     'owns the diff', '2020-01-01T00:00:00Z'),
			(1, 'conv-b', 'reviewer', 'reviewer', '',              '2020-01-02T00:00:00Z'),
			(2, 'conv-a', '',         '',         '',              '2020-01-03T00:00:00Z');
	`)
	require.NoError(t, err, "seed v34 schema")

	require.NoError(t, migrateV34toV35(d), "migrateV34toV35")

	// schema_version bumped to 35.
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 35, ver, "schema_version after migration")

	// The alias column is gone; the rest of the schema survives.
	cols := tableColumns(t, d, "agent_group_members")
	assert.NotContains(t, cols, "alias", "alias column should be dropped; cols=%v", cols)
	assert.Subset(t, cols, []string{"group_id", "conv_id", "role", "descr", "joined_at"},
		"surviving columns; got=%v", cols)

	// Every member row survived with its non-alias fields intact.
	type member struct{ conv, role, descr, joined string }
	byKey := map[string]member{}
	rows, err := d.Query(`SELECT group_id, conv_id, role, descr, joined_at FROM agent_group_members`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var gid int64
		var m member
		require.NoError(t, rows.Scan(&gid, &m.conv, &m.role, &m.descr, &m.joined))
		byKey[itoa(gid)+"/"+m.conv] = m
	}
	require.NoError(t, rows.Err())
	require.Len(t, byKey, 3, "all 3 member rows preserved")
	assert.Equal(t, member{"conv-a", "lead", "owns the diff", "2020-01-01T00:00:00Z"}, byKey["1/conv-a"])
	assert.Equal(t, member{"conv-b", "reviewer", "", "2020-01-02T00:00:00Z"}, byKey["1/conv-b"])
	assert.Equal(t, member{"conv-a", "", "", "2020-01-03T00:00:00Z"}, byKey["2/conv-a"])

	// The (group_id, conv_id) primary key still holds — a duplicate
	// insert is rejected, proving the rebuilt/altered table kept its PK.
	_, err = d.Exec(`INSERT INTO agent_group_members (group_id, conv_id, role, descr, joined_at)
		VALUES (1, 'conv-a', '', '', '2020-02-01T00:00:00Z')`)
	assert.Error(t, err, "PK (group_id, conv_id) should still reject a duplicate")
}

// tableColumns returns the column names of a table via PRAGMA table_info.
func tableColumns(t *testing.T, d *sql.DB, table string) []string {
	t.Helper()
	rows, err := d.Query(`PRAGMA table_info(` + table + `)`)
	require.NoError(t, err, "PRAGMA table_info(%s)", table)
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		cols = append(cols, name)
	}
	require.NoError(t, rows.Err())
	return cols
}

// itoa is a tiny int64→string helper kept local so the test file has
// no extra imports beyond database/sql.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
