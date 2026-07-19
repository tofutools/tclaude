package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV137AgentExitAudit_Idempotent(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, migrateV136toV137(d), "repeated migration converges")

	for table, columns := range map[string][]string{
		"audit_log": {"event_id", "related_event_id", "session_id", "tmux_session", "pane_id", "observer", "cause_kind", "observed_process", "launch_phase", "exit_code", "signal", "lifecycle_action", "reason", "observed_state", "dedup_key"},
		"sessions":  {"exit_intent", "exit_intent_event_id", "exit_intent_generation", "exit_intent_at", "exit_callback_generation", "exit_callback_token_hash", "exit_callback_pane_id", "exit_callback_used_at", "exit_launch_gate_state"},
	} {
		for _, column := range columns {
			var have int
			require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&have))
			assert.Equalf(t, 1, have, "%s.%s", table, column)
		}
	}
}

func TestMigrateV137_FromRealV136FixturePreservesLegacyRowsAndListReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ResetForTest()
	dir := filepath.Join(home, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "db.sqlite"))
	require.NoError(t, err)
	require.NoError(t, createSchema(legacy))
	for _, step := range migrationSteps {
		if step.version > 136 {
			break
		}
		require.NoErrorf(t, step.apply(legacy), "migrate fixture to v%d", step.version)
	}
	require.Equal(t, 136, schemaVersion(legacy), "fixture is the exact post-main-v136 schema")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mustExec(t, legacy, `INSERT INTO sessions (id, tmux_session, conv_id, status, created_at, updated_at)
		VALUES ('legacy-session', 'legacy-tmux', 'legacy-conv', 'working', ?, ?)`, now, now)
	mustExec(t, legacy, `INSERT INTO audit_log
		(at, actor_kind, actor_label, verb, target_label, detail, method, path, status, source)
		VALUES (?, 'human', 'operator', 'message', 'legacy-worker', 'legacy detail', 'POST', '/v1/messages', 200, 'cli')`, now)
	mustExec(t, legacy, `INSERT INTO pending_spawns
		(label, group_id, task_url, task_label, created_at)
		VALUES ('legacy-pending', 7, 'https://linear.app/acme/issue/TCL-568/legacy-task', 'TCL-568', ?)`, now)
	require.NoError(t, legacy.Close())

	d, err := Open()
	require.NoError(t, err)
	assert.Equal(t, currentVersion, schemaVersion(d))
	var status, tmux string
	require.NoError(t, d.QueryRow(`SELECT status, tmux_session FROM sessions WHERE id = 'legacy-session'`).Scan(&status, &tmux))
	assert.Equal(t, "working", status)
	assert.Equal(t, "legacy-tmux", tmux)
	rows, err := ListAuditLog(AuditLogFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "message", rows[0].Verb)
	assert.Equal(t, "legacy detail", rows[0].Detail)
	assert.Empty(t, rows[0].ObservedProcess)
	assert.Empty(t, rows[0].LaunchPhase)
	assert.Nil(t, rows[0].ExitCode)
	pending, err := GetPendingSpawn("legacy-pending")
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, "https://linear.app/acme/issue/TCL-568/legacy-task", pending.TaskURL)
	assert.Equal(t, "TCL-568", pending.TaskLabel)
}
