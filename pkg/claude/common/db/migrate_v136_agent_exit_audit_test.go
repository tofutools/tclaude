package db

import (
	"testing"

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
		"audit_log": {"event_id", "related_event_id", "session_id", "tmux_session", "pane_id", "observer", "cause_kind", "exit_code", "signal", "lifecycle_action", "reason", "observed_state", "dedup_key"},
		"sessions":  {"exit_intent", "exit_intent_event_id", "exit_intent_generation", "exit_intent_at", "exit_callback_generation", "exit_callback_token_hash", "exit_callback_pane_id", "exit_callback_used_at"},
	} {
		for _, column := range columns {
			var have int
			require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&have))
			assert.Equalf(t, 1, have, "%s.%s", table, column)
		}
	}
}
