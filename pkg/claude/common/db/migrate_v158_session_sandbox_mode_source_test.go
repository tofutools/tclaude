package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV157toV158AddsSandboxModeSourceColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN sandbox_mode_source`)
	mustExec(t, d, `UPDATE schema_version SET version = 157`)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status, created_at, updated_at)
		VALUES ('legacy-session', 'tc-legacy', 0, '/tmp', 'conv-legacy', 'idle',
		        '2026-07-25T09:00:00Z', '2026-07-25T09:00:00Z')`)

	require.NoError(t, migrateV157toV158(d))

	// A legacy row reads as "nothing recorded", never as "explicit". Crediting
	// a pre-column launch to the operator would be exactly the false attribution
	// the column exists to remove.
	var source string
	require.NoError(t, d.QueryRow(
		`SELECT sandbox_mode_source FROM sessions WHERE id = 'legacy-session'`).Scan(&source))
	assert.Empty(t, source)

	assert.Equal(t, 158, schemaVersion(d))
	require.NoError(t, migrateV157toV158(d), "partially applied migration converges")
}

// The durability contract, matching the verdict columns beside it: the
// attribution survives the load→mutate→save cycle every state-tracking hook
// performs. Unlike the verdict, it follows its MODE rather than being preserved
// on a blank save — a relaunch that re-chose the mode with nothing to attribute
// must erase the old attribution rather than let it survive onto a mode whose
// tier never chose it.
func TestSessionSandboxModeSourceRoundTrips(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, SaveSession(&SessionRow{
		ID: "s1", ConvID: "c1", Status: "running", Harness: DefaultHarness,
		SandboxMode: "on", SandboxModeSource: `global default profile "agents"`,
	}))

	got, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, `global default profile "agents"`, got.SandboxModeSource)

	require.NoError(t, SaveSession(&SessionRow{
		ID: "s1", ConvID: "c1", Status: "running", Harness: DefaultHarness,
		SandboxMode: "on",
	}))
	cleared, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Empty(t, cleared.SandboxModeSource,
		"a launch with nothing to attribute must not inherit the previous launch's chooser")
}
