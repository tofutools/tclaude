package db

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// schemaGoldenPath is the committed canonical schema snapshot, regenerated
// from a fresh fully-migrated DB. See TestSchemaSnapshot.
const schemaGoldenPath = "schema.sql"

// freshMigratedDB builds a brand-new DB by running the migration chain
// directly, returning its handle at currentVersion.
//
// It deliberately bypasses Open()'s VACUUM-template fast-path: VACUUM renumbers
// sqlite_master.rowid, which would scramble the creation-order ordering that
// SchemaSQL (and thus the golden snapshot) depends on. A direct migrate()
// preserves true creation order and is deterministic across runs.
func freshMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	path := t.TempDir() + "/schema.sqlite"
	d, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
	require.NoError(t, err, "open")
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, migrate(d), "migrate")
	return d
}

// TestSchemaSnapshot is the golden test: the committed schema.sql must equal
// the schema produced by a fresh fully-migrated DB. It fails (loudly, with a
// regeneration hint) whenever a migration changes the schema without the
// snapshot being refreshed, so every schema delta shows up in the PR diff.
//
// Regenerate after an intentional schema change:
//
//	TCLAUDE_UPDATE_SCHEMA_GOLDEN=1 go test ./pkg/claude/common/db/ -run TestSchemaSnapshot
func TestSchemaSnapshot(t *testing.T) {
	d := freshMigratedDB(t)

	got, err := SchemaSQL(d)
	require.NoError(t, err, "SchemaSQL")

	if os.Getenv("TCLAUDE_UPDATE_SCHEMA_GOLDEN") != "" {
		require.NoError(t, os.WriteFile(schemaGoldenPath, []byte(got), 0644), "write golden")
		t.Logf("updated %s (%d bytes)", schemaGoldenPath, len(got))
		return
	}

	want, err := os.ReadFile(schemaGoldenPath)
	require.NoError(t, err, "read golden %s (regenerate with TCLAUDE_UPDATE_SCHEMA_GOLDEN=1)", schemaGoldenPath)

	require.Equalf(t, string(want), got,
		"%s is stale (schema changed without regenerating the snapshot).\n"+
			"Regenerate with:\n  TCLAUDE_UPDATE_SCHEMA_GOLDEN=1 go test ./pkg/claude/common/db/ -run TestSchemaSnapshot",
		schemaGoldenPath)
}

// TestSchemaStructured sanity-checks the structured (--json) form: a known
// table is present with its columns, and the identity classifier tags the
// agent_messages conv/agent columns the FK audit cares about.
func TestSchemaStructured(t *testing.T) {
	d := freshMigratedDB(t)

	info, err := SchemaStructured(d)
	require.NoError(t, err, "SchemaStructured")
	require.Equal(t, currentVersion, info.SchemaVersion, "schema_version")
	require.NotEmpty(t, info.Tables, "expected tables")

	byName := map[string]SchemaTable{}
	for _, tbl := range info.Tables {
		byName[tbl.Name] = tbl
	}

	msgs, ok := byName["agent_messages"]
	require.True(t, ok, "agent_messages table present")

	ident := map[string]string{}
	for _, c := range msgs.Columns {
		ident[c.Name] = c.Identity
	}
	require.Equal(t, "conv", ident["from_conv"], "from_conv -> conv")
	require.Equal(t, "conv", ident["to_conv"], "to_conv -> conv")
	require.Equal(t, "agent", ident["from_agent"], "from_agent -> agent")
	require.Equal(t, "agent", ident["to_agent"], "to_agent -> agent")
}

func TestSchemaTimestampColumnsAreInteger(t *testing.T) {
	d := freshMigratedDB(t)
	info, err := SchemaStructured(d)
	require.NoError(t, err)
	strictTables := map[string]bool{}
	tableRows, err := d.Query(`SELECT name, strict FROM pragma_table_list WHERE type = 'table'`)
	require.NoError(t, err)
	for tableRows.Next() {
		var name string
		var strict bool
		require.NoError(t, tableRows.Scan(&name, &strict))
		strictTables[name] = strict
	}
	require.NoError(t, tableRows.Close())
	require.NoError(t, tableRows.Err())

	var timestampColumns []string
	var timestampNames []string
	seenNames := map[string]bool{}
	for _, table := range info.Tables {
		for _, column := range table.Columns {
			if !isTimestampColumn(column.Name) {
				continue
			}
			require.True(t, strictTables[table.Name], "%s timestamp table must reject TEXT storage", table.Name)
			qualified := table.Name + "." + column.Name
			timestampColumns = append(timestampColumns, qualified)
			if !seenNames[column.Name] {
				seenNames[column.Name] = true
				timestampNames = append(timestampNames, column.Name)
			}
			require.Equal(t, "INTEGER", column.Type, qualified)
		}
	}
	require.NotEmpty(t, timestampColumns)
	t.Logf("timestamp columns (%d): %v", len(timestampColumns), timestampColumns)

	rows, err := d.Query(`SELECT type, name, sql FROM sqlite_master
		WHERE type IN ('table', 'index', 'trigger', 'view') AND sql IS NOT NULL`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var objectType, name, statement string
		require.NoError(t, rows.Scan(&objectType, &name, &statement))
		require.Equalf(t, statement, rewriteTimestampSentinelPredicates(statement, timestampNames),
			"%s %s retains an empty-string timestamp predicate", objectType, name)
	}
	require.NoError(t, rows.Err())
}

// TestClassifyIdentityColumn pins the conv/agent column-name classifier.
func TestClassifyIdentityColumn(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"agent_id", "agent"},
		{"from_agent", "agent"},
		{"to_agent", "agent"},
		{"actor_agent_id", "agent"},
		{"current_agent_id", "agent"},
		{"conv_id", "conv"},
		{"from_conv", "conv"},
		{"to_conv", "conv"},
		{"current_conv_id", "conv"},
		{"actor_conv", "conv"},
		{"name", ""},
		{"created_at", ""},
		{"status", ""},
	}
	for _, tc := range cases {
		require.Equalf(t, tc.want, classifyIdentityColumn(tc.name), "classify %q", tc.name)
	}
}
