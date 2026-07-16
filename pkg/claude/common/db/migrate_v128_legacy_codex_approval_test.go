package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV127toV128BackfillsOnlyProvableCodexPostures(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v128?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	mustExec(t, d, `CREATE TABLE schema_version (version INTEGER NOT NULL)`)
	mustExec(t, d, `INSERT INTO schema_version VALUES (127)`)
	mustExec(t, d, `CREATE TABLE sessions (
		id TEXT PRIMARY KEY, conv_id TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '',
		harness TEXT NOT NULL, approval_policy TEXT NOT NULL DEFAULT '',
		approval_auto_review INTEGER NOT NULL DEFAULT 0)`)
	mustExec(t, d, `CREATE TABLE agents (
		agent_id TEXT PRIMARY KEY, created_via TEXT NOT NULL DEFAULT '',
		initial_spawn_config TEXT NOT NULL DEFAULT '')`)
	mustExec(t, d, `CREATE TABLE agent_conversations (
		conv_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '')`)

	type seeded struct{ id, via, reason, config string }
	for _, row := range []seeded{
		{"omitted", "spawn", "spawn", `{"harness":"codex"}`},
		{"explicit-never", "spawn", "spawn", `{"harness":"codex","approval":"never"}`},
		{"successor", "spawn", "reincarnate", `{"harness":"codex","approval":"untrusted"}`},
		{"clone", "clone", "clone", ``},
		{"explicit-ambiguous", "spawn", "spawn", `{"harness":"codex","approval":"untrusted"}`},
		{"template-no-snapshot", "spawn", "spawn", ``},
		{"direct", "cli", "cli", ``},
		{"invalid-snapshot", "spawn", "spawn", `{`},
	} {
		agentID, convID := "agt-"+row.id, "conv-"+row.id
		mustExec(t, d, `INSERT INTO agents (agent_id, created_via, initial_spawn_config) VALUES (?, ?, ?)`, agentID, row.via, row.config)
		mustExec(t, d, `INSERT INTO agent_conversations (conv_id, agent_id, reason) VALUES (?, ?, ?)`, convID, agentID, row.reason)
		mustExec(t, d, `INSERT INTO sessions (id, conv_id, agent_id, harness) VALUES (?, ?, ?, 'codex')`, row.id, convID, agentID)
	}
	mustExec(t, d, `INSERT INTO sessions (id, conv_id, harness) VALUES ('unmapped', 'conv-unmapped', 'codex')`)

	require.NoError(t, migrateV127toV128(d))
	for _, id := range []string{"omitted", "explicit-never", "successor", "clone"} {
		var policy string
		require.NoError(t, d.QueryRow(`SELECT approval_policy FROM sessions WHERE id = ?`, id).Scan(&policy))
		assert.Equal(t, legacyCodexApprovalDefault, policy, id)
	}
	for _, id := range []string{"explicit-ambiguous", "template-no-snapshot", "direct", "invalid-snapshot", "unmapped"} {
		var policy string
		require.NoError(t, d.QueryRow(`SELECT approval_policy FROM sessions WHERE id = ?`, id).Scan(&policy))
		assert.Empty(t, policy, id+" must remain fail-closed")
	}
	assert.Equal(t, 128, schemaVersion(d))
}

func TestInferLegacyCodexApprovalRequiresDeterministicProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, via, reason, config string
		ok                        bool
	}{
		{"omitted daemon default", "spawn", "spawn", `{}`, true},
		{"explicit never", "spawn", "spawn", `{"approval":"never"}`, true},
		{"successor default", "spawn", "clear", `{"approval":"on-request"}`, true},
		{"clone default", "clone", "clone", ``, true},
		{"original explicit policy may have resumed", "spawn", "spawn", `{"approval":"on-request"}`, false},
		{"template lacks request snapshot", "spawn", "spawn", ``, false},
		{"direct session", "cli", "cli", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, ok := inferLegacyCodexApproval(tc.via, tc.reason, tc.config)
			assert.Equal(t, tc.ok, ok)
			if ok {
				assert.Equal(t, legacyCodexApprovalDefault, policy)
			} else {
				assert.Empty(t, policy)
			}
		})
	}
}
