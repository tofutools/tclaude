package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV126toV127AddsSessionApprovalPosture(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v127?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (126)`)
	mustExec(t, d, `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	mustExec(t, d, `INSERT INTO sessions (id) VALUES ('existing')`)

	require.NoError(t, migrateV126toV127(d))
	var policy string
	var autoReview int
	require.NoError(t, d.QueryRow(`SELECT approval_policy, approval_auto_review FROM sessions WHERE id = 'existing'`).Scan(&policy, &autoReview))
	assert.Empty(t, policy)
	assert.Zero(t, autoReview)
	assert.Equal(t, 127, schemaVersion(d))
	require.NoError(t, migrateV126toV127(d), "migration is idempotent after a partially applied schema change")
}

func TestSessionApprovalPostureRoundTrip(t *testing.T) {
	setupTestDB(t)
	require.Equal(t, 128, currentVersion, "tripwire: bump this with the next migration")
	require.NoError(t, SaveSession(&SessionRow{
		ID: "approval-session", ConvID: "approval-conv", Status: "running",
		Harness: "claude", ApprovalPolicy: "default", ApprovalAutoReview: true,
	}))

	got, err := LoadSession("approval-session")
	require.NoError(t, err)
	assert.Equal(t, "default", got.ApprovalPolicy)
	assert.True(t, got.ApprovalAutoReview)

	got.Status = "idle"
	require.NoError(t, SaveSession(got))
	again, err := LoadSession("approval-session")
	require.NoError(t, err)
	assert.Equal(t, "default", again.ApprovalPolicy)
	assert.True(t, again.ApprovalAutoReview)

	// Hook/status updates may not know immutable launch posture. A blank
	// update preserves the recorded pair rather than erasing the guard's
	// evidence.
	require.NoError(t, SaveSession(&SessionRow{ID: "approval-session", ConvID: "approval-conv", Status: "working"}))
	afterHook, err := LoadSession("approval-session")
	require.NoError(t, err)
	assert.Equal(t, "default", afterHook.ApprovalPolicy)
	assert.True(t, afterHook.ApprovalAutoReview)
}
