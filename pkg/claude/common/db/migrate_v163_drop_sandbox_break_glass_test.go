package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedV162SandboxProfiles builds a v162-shaped sandbox_profiles table — the
// exact columns the registry had before TCL-791 — so the migration is exercised
// against real persisted break-glass data rather than a synthetic stub.
func seedV162SandboxProfiles(t *testing.T, d *sql.DB) {
	t.Helper()
	_, err := d.Exec(`CREATE TABLE sandbox_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		filesystem_json TEXT NOT NULL DEFAULT '[]',
		environment_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		includes_json TEXT NOT NULL DEFAULT '[]',
		agent_directories_json TEXT NOT NULL DEFAULT '[]',
		network_access TEXT NOT NULL DEFAULT '',
		break_glass_filesystem_json TEXT NOT NULL DEFAULT '[]')`)
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TABLE IF NOT EXISTS human_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		from_conv TEXT NOT NULL,
		from_title TEXT NOT NULL DEFAULT '',
		group_name TEXT NOT NULL DEFAULT '',
		subject TEXT NOT NULL DEFAULT '',
		body TEXT NOT NULL,
		created_at TEXT NOT NULL,
		read_at TEXT NOT NULL DEFAULT '',
		from_agent TEXT NOT NULL DEFAULT '',
		process_run_id TEXT NOT NULL DEFAULT '',
		process_node_id TEXT NOT NULL DEFAULT '',
		process_command_id TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)
	// Deliberately NOT creating agent_conversations: the migration writes its
	// disclosure with frozen inline SQL touching human_messages only. If someone
	// later routes it through a live helper that joins other tables, these tests
	// fail with "no such table" — which is the point.
	_, err = d.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`)
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO schema_version (version) VALUES (162)`)
	require.NoError(t, err)
}

func openV162DB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", t.TempDir()+"/v162.sqlite")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	seedV162SandboxProfiles(t, d)
	return d
}

func hasColumn(t *testing.T, d *sql.DB, table, column string) bool {
	t.Helper()
	exists, err := columnExists(d, table, column)
	require.NoError(t, err)
	return exists
}

// TestMigrateV163DropsBreakGlassWithoutWidening is the anti-widening proof
// constraint 1 of TCL-791 demands. The break-glass grant must vanish and the
// ordinary filesystem rules must come out BYTE-IDENTICAL: a translated rule
// would reopen a protected root as an ordinary grant, which is the exact
// privilege escalation this ticket exists to prevent.
func TestMigrateV163DropsBreakGlassWithoutWidening(t *testing.T) {
	d := openV162DB(t)

	const ordinaryFilesystem = `[{"path":"/home/dev/work","access":"write"}]`
	const breakGlass = `[{"path":"/home/dev/.tclaude/data","access":"write"},` +
		`{"path":"/home/dev/.claude/sessions","access":"read"}]`
	_, err := d.Exec(`INSERT INTO sandbox_profiles
		(name, filesystem_json, environment_json, includes_json, agent_directories_json,
		 network_access, break_glass_filesystem_json, created_at, updated_at)
		VALUES ('debugger', ?, '[]', '[]', '[]', '', ?, 1767225600000000000, 1767225600000000000)`,
		ordinaryFilesystem, breakGlass)
	require.NoError(t, err)

	require.NoError(t, migrateV162toV163(d))

	// (a) the column is gone.
	assert.False(t, hasColumn(t, d, "sandbox_profiles", "break_glass_filesystem_json"),
		"break_glass_filesystem_json must be dropped")

	// (b) the ordinary rules are byte-identical — NO rule appeared. This is the
	// assertion that fails if anyone ever "helpfully" translates the grants.
	var gotFilesystem string
	require.NoError(t, d.QueryRow(
		`SELECT filesystem_json FROM sandbox_profiles WHERE name = 'debugger'`).Scan(&gotFilesystem))
	assert.Equal(t, ordinaryFilesystem, gotFilesystem,
		"filesystem_json changed; break-glass grants must be DROPPED, never migrated into ordinary rules")

	// (c) the row survives and still reads back through the production loader.
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 163, version)

	// (d) exactly one durable disclosure, naming every dropped rule.
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM human_messages`).Scan(&count))
	require.Equal(t, 1, count, "the drop must be disclosed exactly once")

	var subject, body, fromTitle string
	require.NoError(t, d.QueryRow(
		`SELECT subject, body, from_title FROM human_messages`).Scan(&subject, &body, &fromTitle))
	assert.Equal(t, breakGlassDropSubject, subject)
	assert.Equal(t, breakGlassDropSender, fromTitle)
	for _, want := range []string{
		"debugger",
		"write /home/dev/.tclaude/data",
		"read /home/dev/.claude/sessions",
		"NOT converted",
		"disabling the sandbox",
	} {
		assert.Contains(t, body, want, "the disclosure must name the real reason and the exact loss")
	}
}

// TestMigrateV163IsIdempotent proves a re-run neither fails nor discloses
// again. A second "your rules were dropped" message on every restart would
// train the operator to ignore the channel.
func TestMigrateV163IsIdempotent(t *testing.T) {
	d := openV162DB(t)
	_, err := d.Exec(`INSERT INTO sandbox_profiles
		(name, filesystem_json, environment_json, includes_json, agent_directories_json,
		 network_access, break_glass_filesystem_json, created_at, updated_at)
		VALUES ('debugger', '[]', '[]', '[]', '[]', '',
		        '[{"path":"/home/dev/.tclaude/data","access":"read"}]', 1767225600000000000, 1767225600000000000)`)
	require.NoError(t, err)

	require.NoError(t, migrateV162toV163(d))
	require.NoError(t, migrateV162toV163(d), "re-running the migration must be a no-op, not an error")

	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM human_messages`).Scan(&count))
	assert.Equal(t, 1, count, "a re-run must not disclose the drop a second time")
}

// TestMigrateV163StaysSilentWithoutBreakGlass covers the clean install. An
// operator who never used the feature must not be told it was removed.
func TestMigrateV163StaysSilentWithoutBreakGlass(t *testing.T) {
	d := openV162DB(t)
	_, err := d.Exec(`INSERT INTO sandbox_profiles
		(name, filesystem_json, environment_json, includes_json, agent_directories_json,
		 network_access, break_glass_filesystem_json, created_at, updated_at)
		VALUES ('ordinary', '[{"path":"/home/dev/work","access":"write"}]', '[]', '[]', '[]', '', '[]', 1767225600000000000, 1767225600000000000)`)
	require.NoError(t, err)

	require.NoError(t, migrateV162toV163(d))

	assert.False(t, hasColumn(t, d, "sandbox_profiles", "break_glass_filesystem_json"))
	var count int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM human_messages`).Scan(&count))
	assert.Zero(t, count, "an install that never used break-glass must not be told it was removed")
}

// TestMigrateV163DisclosureNotifiesTheTerminal covers the second channel: the
// startup line an operator sees even if they never open the Messages tab.
func TestMigrateV163DisclosureNotifiesTheTerminal(t *testing.T) {
	var notices []string
	SetMigrationReporter(&MigrationReporter{
		Notice: func(version int, message string) {
			assert.Equal(t, 163, version)
			notices = append(notices, message)
		},
	})
	t.Cleanup(func() { SetMigrationReporter(nil) })

	d := openV162DB(t)
	_, err := d.Exec(`INSERT INTO sandbox_profiles
		(name, filesystem_json, environment_json, includes_json, agent_directories_json,
		 network_access, break_glass_filesystem_json, created_at, updated_at)
		VALUES ('debugger', '[]', '[]', '[]', '[]', '',
		        '[{"path":"/home/dev/.tclaude/data","access":"write"}]', 1767225600000000000, 1767225600000000000)`)
	require.NoError(t, err)

	require.NoError(t, migrateV162toV163(d))

	require.Len(t, notices, 1, "the terminal channel must fire exactly once")
	assert.True(t, strings.Contains(notices[0], "write /home/dev/.tclaude/data"),
		"the terminal notice must name the dropped rule, not just say something was removed")
}

// TestMigrateV163SurvivesUnreadableBreakGlassJSON proves the upgrade is not
// blockable by data the operator can no longer use anyway. The column is going
// either way; failing here would strand them on the old schema for nothing.
func TestMigrateV163SurvivesUnreadableBreakGlassJSON(t *testing.T) {
	d := openV162DB(t)
	_, err := d.Exec(`INSERT INTO sandbox_profiles
		(name, filesystem_json, environment_json, includes_json, agent_directories_json,
		 network_access, break_glass_filesystem_json, created_at, updated_at)
		VALUES ('corrupt', '[]', '[]', '[]', '[]', '', 'not valid json', 1767225600000000000, 1767225600000000000)`)
	require.NoError(t, err)

	require.NoError(t, migrateV162toV163(d))
	assert.False(t, hasColumn(t, d, "sandbox_profiles", "break_glass_filesystem_json"))
	assert.Equal(t, 191, currentVersion, "tripwire: bump this with the next migration")
}
