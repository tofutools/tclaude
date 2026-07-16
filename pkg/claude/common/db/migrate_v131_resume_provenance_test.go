package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV130toV131AddsEmptySessionResumeProvenance(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v131?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (130)`)
	mustExec(t, d, `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	mustExec(t, d, `INSERT INTO sessions (id) VALUES ('legacy-stopped')`)

	require.NoError(t, migrateV130toV131(d))
	var raw string
	require.NoError(t, d.QueryRow(`SELECT resume_provenance FROM sessions WHERE id = 'legacy-stopped'`).Scan(&raw))
	assert.Empty(t, raw, "offline legacy pathnames must not be backfilled as trustworthy identity")
	assert.Equal(t, 131, schemaVersion(d))
	require.NoError(t, migrateV130toV131(d), "migration must converge after a partially applied schema change")
}

func TestSessionResumeProvenanceRoundTripPreserveAndExplicitClear(t *testing.T) {
	setupTestDB(t)
	const raw = `{"version":1,"cwd":{"path":"/tmp/target","device":1,"inode":2},"repository_state":"none"}`
	require.NoError(t, SaveSession(&SessionRow{
		ID: "resume-session", ConvID: "resume-conv", Status: "running", ResumeProvenance: raw,
	}))

	got, err := LoadSession("resume-session")
	require.NoError(t, err)
	assert.Equal(t, raw, got.ResumeProvenance)

	// Hook upserts do not carry launch-only identity and must preserve it.
	require.NoError(t, SaveSession(&SessionRow{ID: "resume-session", ConvID: "resume-conv", Status: "idle"}))
	got, err = LoadSession("resume-session")
	require.NoError(t, err)
	assert.Equal(t, raw, got.ResumeProvenance)

	// Controlled-stop capture failure uses the explicit setter because empty is
	// a security state there, not "field omitted".
	require.NoError(t, SetSessionResumeProvenance("resume-session", ""))
	got, err = LoadSession("resume-session")
	require.NoError(t, err)
	assert.Empty(t, got.ResumeProvenance)

	// A hook that loaded the row before invalidation may still carry the old
	// value in memory. SaveSession never owns updates to this column, so the
	// stale tick cannot resurrect trust after stop cleared it.
	stale := &SessionRow{
		ID: "resume-session", ConvID: "resume-conv", Status: "exited", ResumeProvenance: raw,
	}
	require.NoError(t, SaveSession(stale))
	got, err = LoadSession("resume-session")
	require.NoError(t, err)
	assert.Empty(t, got.ResumeProvenance)
}

func TestSessionResumeProvenanceLaunchBoundaryWinsHookInsertRace(t *testing.T) {
	setupTestDB(t)
	const raw = `{"version":1,"cwd":{"path":"/tmp/target","device":1,"inode":2},"repository_state":"none"}`

	// SessionStart can create the row before the launch parent has captured the
	// pane's physical identity. A generic UPSERT cannot be allowed to own the
	// column because the same path is also used by stale post-stop hooks.
	require.NoError(t, SaveSession(&SessionRow{ID: "launch-race", Status: "running"}))
	require.NoError(t, SaveSession(&SessionRow{
		ID: "launch-race", Status: "idle", ResumeProvenance: raw,
	}))
	row, err := LoadSession("launch-race")
	require.NoError(t, err)
	assert.Empty(t, row.ResumeProvenance)

	// The trusted launch boundary performs the explicit replacement after its
	// state UPSERT, exactly as session new does in production.
	require.NoError(t, SetSessionResumeProvenance("launch-race", raw))
	row, err = LoadSession("launch-race")
	require.NoError(t, err)
	assert.Equal(t, raw, row.ResumeProvenance)
}
