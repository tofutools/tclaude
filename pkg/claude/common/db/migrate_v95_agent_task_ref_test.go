package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV94toV95_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts it lands at currentVersion. The literal currentVersion
// tripwire has moved forward to the v98 test (migrate_v98_session_ask_timeout_test.go);
// this one just checks the fresh chain reaches head.
func TestMigrateV94toV95_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")
}

// TestMigrateV94toV95_AddsColumns drives the real v94→v95 ALTER over a
// v94-pinned DB: it asserts agents.task_ref_url / task_ref_label appear, that a
// pre-existing row reads back as "" (no task link), that the version advances,
// and that a re-run is a clean no-op.
func TestMigrateV94toV95_AddsColumns(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	// Pin back to v94 and drop the new columns so we re-add them from a true v94
	// shape (the fresh chain already ran v95). SQLite supports DROP COLUMN.
	mustExec(t, d, `ALTER TABLE agents DROP COLUMN task_ref_url`)
	mustExec(t, d, `ALTER TABLE agents DROP COLUMN task_ref_label`)
	mustExec(t, d, `UPDATE schema_version SET version = 94`)

	// A pre-existing agent row (without the new columns) must survive the ALTER
	// and read back with the defaults.
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_legacy', 'conv-legacy', '2026-07-02T00:00:00Z')`)

	require.NoError(t, migrateV94toV95(d), "v94→v95")

	for _, col := range []string{"task_ref_url", "task_ref_label"} {
		var n int
		require.NoError(t, d.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = ?`, col).Scan(&n))
		assert.Equal(t, 1, n, "agents."+col+" added")
	}

	var url, label string
	require.NoError(t, d.QueryRow(
		`SELECT task_ref_url, task_ref_label FROM agents WHERE agent_id = 'agt_legacy'`).
		Scan(&url, &label))
	assert.Equal(t, "", url, "existing row defaults to no task link")
	assert.Equal(t, "", label, "existing row defaults to no task label")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 95, ver, "version advanced")

	// Idempotent: a re-run over the already-migrated DB is a clean no-op (the
	// pragma guards skip the duplicate ADD COLUMNs).
	require.NoError(t, migrateV94toV95(d), "v94→v95 re-run is a clean no-op")
}

// TestSetAgentTaskRef covers the set / clear / get round-trip on the per-agent
// task-reference columns, including the "clearing the URL clears the label"
// rule and the no-such-agent no-op.
func TestSetAgentTaskRef(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_x', 'conv-x', '2026-07-02T00:00:00Z')`)

	// Set with an explicit label.
	n, err := SetAgentTaskRef("agt_x", "https://linear.app/a/issue/JOH-1", "custom")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	ref, err := GetAgentTaskRef("agt_x")
	require.NoError(t, err)
	assert.Equal(t, "https://linear.app/a/issue/JOH-1", ref.URL)
	assert.Equal(t, "custom", ref.Label)

	// Clearing the URL clears the label too.
	_, err = SetAgentTaskRef("agt_x", "", "")
	require.NoError(t, err)
	ref, err = GetAgentTaskRef("agt_x")
	require.NoError(t, err)
	assert.Equal(t, "", ref.URL)
	assert.Equal(t, "", ref.Label)

	// Unknown agent: a no-op (0 rows), not an error.
	n, err = SetAgentTaskRef("agt_missing", "https://x.io", "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// GetAgentTaskRef on a missing agent is ("", "", nil).
	ref, err = GetAgentTaskRef("agt_missing")
	require.NoError(t, err)
	assert.Equal(t, AgentTaskRef{}, ref)
}

// TestListAgentTaskRefs returns only agents with a non-empty URL, keyed by id.
func TestListAgentTaskRefs(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at) VALUES ('agt_a', 'conv-a', '2026-07-02T00:00:00Z')`)
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at) VALUES ('agt_b', 'conv-b', '2026-07-02T00:00:00Z')`)
	_, err = SetAgentTaskRef("agt_a", "https://github.com/o/r/pull/9", "")
	require.NoError(t, err)

	m, err := ListAgentTaskRefs()
	require.NoError(t, err)
	assert.Len(t, m, 1, "only agents with a URL are listed")
	assert.Equal(t, "https://github.com/o/r/pull/9", m["agt_a"].URL)
	_, hasB := m["agt_b"]
	assert.False(t, hasB, "agent with no task link is omitted")
}

func TestListAgentTaskRefsByAgentIDs(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")
	for _, row := range []struct {
		agentID string
		convID  string
	}{
		{"agt_visible_linked", "conv-visible-linked"},
		{"agt_visible_unlinked", "conv-visible-unlinked"},
		{"agt_hidden_linked", "conv-hidden-linked"},
	} {
		mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at) VALUES (?, ?, '2026-07-02T00:00:00Z')`, row.agentID, row.convID)
	}
	_, err = SetAgentTaskRef("agt_visible_linked", "https://linear.app/a/issue/TCL-1", "TCL-1")
	require.NoError(t, err)
	_, err = SetAgentTaskRef("agt_hidden_linked", "https://linear.app/a/issue/TCL-2", "TCL-2")
	require.NoError(t, err)

	refs, err := ListAgentTaskRefsByAgentIDs([]string{"agt_visible_linked", "agt_visible_unlinked"})
	require.NoError(t, err)
	assert.Equal(t, map[string]AgentTaskRef{
		"agt_visible_linked": {URL: "https://linear.app/a/issue/TCL-1", Label: "TCL-1"},
	}, refs)

	refs, err = ListAgentTaskRefsByAgentIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, refs)
}
