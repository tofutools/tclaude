package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A v301 DB with scribe replies stored loses the table, index and rows; the
// rest of the DB is untouched.
func TestMigrateV301toV302DropsAgentTurnReplies(t *testing.T) {
	d := freshMigratedDB(t)
	_, err := d.Exec(`UPDATE schema_version SET version = 301;
		CREATE TABLE agent_turn_replies (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id TEXT NOT NULL,
			text TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX idx_agent_turn_replies_agent ON agent_turn_replies(agent_id, id);
		INSERT INTO agent_turn_replies (agent_id, text) VALUES ('agt_scribe', 'Done: drafted the process.')`)
	require.NoError(t, err)

	require.NoError(t, migrateV301toV302(d))

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 302, version)
	var left int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('agent_turn_replies', 'idx_agent_turn_replies_agent')`).Scan(&left))
	assert.Zero(t, left, "the table and its index are gone")

	// Run again (a DB that never had it) it is a no-op.
	_, err = d.Exec(`UPDATE schema_version SET version = 301`)
	require.NoError(t, err)
	require.NoError(t, migrateV301toV302(d))
}

// A fresh DB at the current version has no agent_turn_replies.
func TestFreshDBHasNoAgentTurnReplies(t *testing.T) {
	var n int
	require.NoError(t, freshMigratedDB(t).QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'agent_turn_replies'`).Scan(&n))
	assert.Zero(t, n)
}
