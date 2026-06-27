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

// freshMigratedDB resets to a clean temp $HOME and opens (thus migrates) a
// brand-new DB, returning its handle at currentVersion.
func freshMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	require.NotNil(t, d, "Open returned nil")
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
