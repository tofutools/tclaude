package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV156toV157AddsOSSandboxColumns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)

	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN os_sandbox_state`)
	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN os_sandbox_source`)
	mustExec(t, d, `ALTER TABLE sessions DROP COLUMN os_sandbox_unverified`)
	mustExec(t, d, `UPDATE schema_version SET version = 156`)
	mustExec(t, d, `INSERT INTO sessions (id, tmux_session, pid, cwd, conv_id, status, created_at, updated_at)
		VALUES ('legacy-session', 'tc-legacy', 0, '/tmp', 'conv-legacy', 'idle',
		        1784970000000000000, 1784970000000000000)`)

	require.NoError(t, migrateV156toV157(d))

	// A legacy row reads as "no verdict recorded" — NOT as "unsandboxed". The
	// launch that created it never resolved the question, and the dashboard
	// renders empty exactly as it did before the columns existed rather than
	// retroactively claiming the agent was or was not confined.
	var state, source string
	var unverified int
	require.NoError(t, d.QueryRow(
		`SELECT os_sandbox_state, os_sandbox_source, os_sandbox_unverified FROM sessions WHERE id = 'legacy-session'`).
		Scan(&state, &source, &unverified))
	assert.Empty(t, state)
	assert.Empty(t, source)
	assert.Zero(t, unverified)

	assert.Equal(t, 157, schemaVersion(d))
	require.NoError(t, migrateV156toV157(d), "partially applied migration converges")
}

// TestSessionOSSandboxVerdictRoundTrips pins the durability contract: the
// launch-time verdict survives the load→mutate→save cycle every state-tracking
// hook performs, and a save that carries no verdict PRESERVES the recorded one
// instead of blanking it. Without the second half, the first hook tick after a
// spawn would erase the badge the spawn just earned.
func TestSessionOSSandboxVerdictRoundTrips(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, SaveSession(&SessionRow{
		ID: "s1", ConvID: "c1", Status: "running", Harness: DefaultHarness,
		HarnessBuiltinMode: "", OSSandboxState: "on", OSSandboxSource: "~/.claude/settings.json",
		OSSandboxUnverified: true,
	}))

	got, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, "on", got.OSSandboxState)
	assert.Equal(t, "~/.claude/settings.json", got.OSSandboxSource)
	assert.True(t, got.OSSandboxUnverified, "the doubt is stored with the verdict")

	got.Status = "idle"
	require.NoError(t, SaveSession(got))
	again, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, "on", again.OSSandboxState, "verdict survives load→mutate→save")
	assert.Equal(t, "~/.claude/settings.json", again.OSSandboxSource)

	// A hook that builds a fresh row for the same session carries no verdict;
	// that must not be read as "no longer sandboxed".
	require.NoError(t, SaveSession(&SessionRow{
		ID: "s1", ConvID: "c1", Status: "working", Harness: DefaultHarness,
	}))
	preserved, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, "on", preserved.OSSandboxState, "a verdict-less save preserves the recorded verdict")
	assert.Equal(t, "~/.claude/settings.json", preserved.OSSandboxSource)
	assert.True(t, preserved.OSSandboxUnverified, "and preserves its doubt")

	// A relaunch that genuinely resolved a different posture DOES overwrite it.
	require.NoError(t, SaveSession(&SessionRow{
		ID: "s1", ConvID: "c1", Status: "running", Harness: DefaultHarness,
		OSSandboxState: "unconfigured",
	}))
	replaced, err := LoadSession("s1")
	require.NoError(t, err)
	assert.Equal(t, "unconfigured", replaced.OSSandboxState)
	assert.Empty(t, replaced.OSSandboxSource, "source follows its state, never outliving it")
	assert.False(t, replaced.OSSandboxUnverified,
		"a newly-resolved verdict clears the previous doubt rather than inheriting it")
}
