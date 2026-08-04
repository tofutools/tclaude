package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV157toV158AddsHarnessBuiltinModeSourceColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN sandbox_mode_source`)
	mustExec(t, d, `UPDATE schema_version SET version = 157`)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status, created_at, updated_at)
		VALUES ('legacy-session', 'tc-legacy', 0, '/tmp', 'conv-legacy', 'idle',
		        1784970000000000000, 1784970000000000000)`)

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
func TestSessionHarnessBuiltinModeSourceRoundTrips(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, SaveSession(&SessionRow{
		ID: "s1", ConvID: "c1", Status: "running", Harness: DefaultHarness,
		HarnessBuiltinMode: "on", HarnessBuiltinModeSource: `global default profile "agents"`,
	}))

	got, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, `global default profile "agents"`, got.HarnessBuiltinModeSource)

	require.NoError(t, SaveSession(&SessionRow{
		ID: "s1", ConvID: "c1", Status: "running", Harness: DefaultHarness,
		HarnessBuiltinMode: "on",
	}))
	cleared, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Empty(t, cleared.HarnessBuiltinModeSource,
		"a launch with nothing to attribute must not inherit the previous launch's chooser")
}
