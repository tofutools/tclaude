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

func TestMigrateV136AgentExitAudit_Idempotent(t *testing.T) {
	require.Equal(t, 136, currentVersion, "tripwire: bump this with the next migration")
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, migrateV135toV136(d), "repeated migration converges")

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

func TestMigrateV136_FromRealV135FixturePreservesLegacyRowsAndListReads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ResetForTest()
	dir := filepath.Join(home, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "db.sqlite"))
	require.NoError(t, err)
	mustExec(t, legacy, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, legacy, `INSERT INTO schema_version VALUES (135)`)
	mustExec(t, legacy, `CREATE TABLE sessions (
		id TEXT PRIMARY KEY, tmux_session TEXT NOT NULL DEFAULT '', pid INTEGER NOT NULL DEFAULT 0,
		cwd TEXT NOT NULL DEFAULT '', conv_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'idle',
		status_detail TEXT NOT NULL DEFAULT '', auto_registered INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		context_pct REAL NOT NULL DEFAULT 0, subagent_count INTEGER NOT NULL DEFAULT 0,
		last_hook TEXT NOT NULL DEFAULT '', tokens_input INTEGER NOT NULL DEFAULT 0,
		tokens_output INTEGER NOT NULL DEFAULT 0, context_window_size INTEGER NOT NULL DEFAULT 0,
		nudged_pct REAL NOT NULL DEFAULT 0, exit_reason TEXT, model TEXT NOT NULL DEFAULT '',
		effort_level TEXT NOT NULL DEFAULT '', pending_conv TEXT NOT NULL DEFAULT '',
		cost_usd REAL NOT NULL DEFAULT 0, model_id TEXT NOT NULL DEFAULT '',
		harness TEXT NOT NULL DEFAULT 'claude', sandbox_mode TEXT NOT NULL DEFAULT '',
		remote_control INTEGER NOT NULL DEFAULT 0, virtual_cost_usd REAL NOT NULL DEFAULT 0,
		agent_id TEXT NOT NULL DEFAULT '', last_statusline_json TEXT NOT NULL DEFAULT '',
		subagents_json TEXT NOT NULL DEFAULT '', ask_user_question_timeout TEXT NOT NULL DEFAULT '',
		effective_sandbox_config TEXT NOT NULL DEFAULT '', approval_policy TEXT NOT NULL DEFAULT '',
		approval_auto_review INTEGER NOT NULL DEFAULT 0, resume_provenance TEXT NOT NULL DEFAULT '')`)
	mustExec(t, legacy, `CREATE TABLE audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, actor_kind TEXT NOT NULL DEFAULT '',
		actor_conv TEXT NOT NULL DEFAULT '', actor_label TEXT NOT NULL DEFAULT '', verb TEXT NOT NULL DEFAULT '',
		target_conv TEXT NOT NULL DEFAULT '', target_label TEXT NOT NULL DEFAULT '', group_name TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '', method TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '',
		status INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '',
		actor_agent TEXT NOT NULL DEFAULT '', target_agent TEXT NOT NULL DEFAULT '')`)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mustExec(t, legacy, `INSERT INTO sessions (id, tmux_session, conv_id, status, created_at, updated_at)
		VALUES ('legacy-session', 'legacy-tmux', 'legacy-conv', 'working', ?, ?)`, now, now)
	mustExec(t, legacy, `INSERT INTO audit_log
		(at, actor_kind, actor_label, verb, target_label, detail, method, path, status, source)
		VALUES (?, 'human', 'operator', 'message', 'legacy-worker', 'legacy detail', 'POST', '/v1/messages', 200, 'cli')`, now)
	require.NoError(t, legacy.Close())

	d, err := Open()
	require.NoError(t, err)
	assert.Equal(t, 136, schemaVersion(d))
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
}
