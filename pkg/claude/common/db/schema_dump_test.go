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

// advanceThroughMigrations walks the REGISTRATION LIST, applying every step
// above from, and then asks THE FIXTURE what version it ended up at.
//
// # Why it reads the database rather than its own loop counter
//
// The first version of this helper tracked the last applied step's version in a
// local and asserted THAT equalled currentVersion, under the message "the
// fixture must reach the head the daemon migrates to". That value came entirely
// from migrationSteps. It made a claim about a database it never queried, and
// [TestMigrationStepsAreContiguous] already pins the list's own last version, so
// it could not fail for any reason that test would not have caught first.
//
// A cold review measured the gap. A registered migration that creates its table
// but omits its own `UPDATE schema_version` left the helper reporting v198 while
// the fixture sat at v197, and BOTH parity tests passed. [SchemaSQL] reads only
// sqlite_master DDL, so nothing downstream noticed either — and in production
// that migration re-runs on every open, forever, because migrate() resumes from
// the row rather than from where it got to last time.
//
// Each migration owns its own version bump (see the tail of any migrate_v*.go);
// migrate() only tracks its position in memory. So the row IS the fixture's own
// answer to "did the chain finish", and it is the only thing asserted here that
// is not derived from the list being walked.
//
// # What it replaces, and why by-name chaining was the bug
//
// The schema-parity tests used to advance a hand-upgraded fixture by naming each
// migration in sequence — sixteen require.NoError lines, extended by hand every
// time a migration landed. Those tests exist to catch schema divergence, and
// chaining by name made them BLIND TO THE MOST COMMON CAUSE OF IT: a migration
// that was written and never chained. Until someone extended the list, the
// upgraded and fresh schemas genuinely differed, and the test reported that as a
// failure of whatever change happened to be in flight.
//
// So the fixture is advanced from the same list the production migrate() loop
// walks. A migration that reaches migrationSteps is applied here automatically;
// one that does not reach it is invisible to the daemon too, which is the defect
// [TestEveryMigrationFileIsRegisteredExactlyOnce] exists to catch. Between them
// the two tests say: every migration file is in the list, and the parity tests
// walk the list.
//
// This is the same shape as the incident that produced this rule (TCL-1093): two
// PRs each claimed v195 and the list carried ONE entry for two migrations, so one
// schema change would never have run. The compile error from the duplicate
// function name is the only reason anybody looked.
//
// # What this trade GIVES UP, stated because it is a real loss
//
// The comparison this feeds is upgraded-schema vs fresh-schema, and freshMigratedDB
// builds the fresh side by calling migrate(), which walks migrationSteps. Advancing
// the upgraded side from the same list CORRELATES BOTH SIDES: a migration dropped
// from the list is now missing from fresh and upgraded alike, so the schemas still
// match and the parity test passes. The hand-chain, being an independent
// enumeration, went red on exactly that.
//
// That is a genuine detection lost, and it is not made up for here — it is made up
// for by [TestEveryMigrationIsRegistered] and [TestMigrationStepsAreContiguous],
// which own the "is every migration in the list" question and fail loudly on a
// dropped entry. The trade is: the parity tests stop answering a question two other
// tests answer better, and stop reporting a defect in one PR as a failure of
// whichever PR happens to be in flight.
//
// The direction NOT traded away is the one the ticket was about: a migration that
// exists and never reaches the list is still invisible here, and still invisible to
// the daemon, which is what the registration guards are for.
//
// # Assumptions it rests on
//
// migrationSteps is in version order — pinned by [TestMigrationStepsAreContiguous],
// which asserts migrationSteps[i].version == i+2. And this walk compares every step
// to the fixed from, where production's loop advances its cursor as it goes; the two
// agree for a contiguous list, which is the same test's job. So it walks the same
// LIST as migrate(), not the same LOOP.
func advanceThroughMigrations(t *testing.T, d *sql.DB, from int) {
	t.Helper()
	var applied int
	for _, step := range migrationSteps {
		if step.version <= from {
			continue
		}
		require.NoErrorf(t, step.apply(d),
			"advancing the upgraded fixture through v%d", step.version)
		applied++
	}

	// The positive control. Without it a from at or above the head advances
	// nothing and every assertion below still passes, against a fixture that was
	// never migrated at all.
	require.NotZerof(t, applied,
		"advanceThroughMigrations applied no steps from v%d; the fixture was not "+
			"advanced, so whatever this test compares next is comparing an unmigrated "+
			"database against a fresh one", from)

	var onDisk int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&onDisk),
		"reading the fixture's own schema_version after advancing it")
	require.Equalf(t, currentVersion, onDisk, ""+
		"the fixture reports schema_version %d after being advanced to the head at v%d. "+
		"Every migration is responsible for its OWN `UPDATE schema_version`; migrate() "+
		"only tracks its position in memory, so a step that changes the schema and "+
		"forgets the bump leaves a real database at the old version and RE-RUNS THAT "+
		"STEP ON EVERY OPEN, FOREVER. Nothing else here would see it: the parity "+
		"comparison reads sqlite_master DDL, which the missing bump does not change",
		onDisk, currentVersion)
}
