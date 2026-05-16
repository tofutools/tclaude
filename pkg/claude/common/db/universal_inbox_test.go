package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV35toV36_MakesGroupIDOptional builds a v35-shape
// agent_messages table — group_id is NOT NULL with a foreign key to
// agent_groups — seeds group-routed messages, runs the v36 migration,
// and asserts: the foreign key is gone, every row survives with its
// group_id intact, and a group_id 0 (direct) message can now be
// inserted, which the old foreign key would have rejected.
func TestMigrateV35toV36_MakesGroupIDOptional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v35.sqlite")
	d, err := sql.Open("sqlite", path)
	require.NoError(t, err, "open raw sqlite")
	defer func() { _ = d.Close() }()

	// Minimal pre-v36 schema: schema_version + agent_groups +
	// agent_messages with the legacy
	//   group_id INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE RESTRICT
	// foreign key, plus seeded messages routed through real groups.
	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (35);
		CREATE TABLE agent_groups (
			id   INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE
		);
		CREATE TABLE agent_messages (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id         INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE RESTRICT,
			from_conv        TEXT NOT NULL,
			to_conv          TEXT NOT NULL,
			subject          TEXT NOT NULL DEFAULT '',
			body             TEXT NOT NULL DEFAULT '',
			created_at       TEXT NOT NULL,
			delivered_at     TEXT NOT NULL DEFAULT '',
			read_at          TEXT NOT NULL DEFAULT '',
			parent_id        INTEGER NOT NULL DEFAULT 0,
			to_recipients    TEXT NOT NULL DEFAULT '',
			cc_recipients    TEXT NOT NULL DEFAULT '',
			original_to_conv TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX idx_agent_messages_to_conv ON agent_messages(to_conv, created_at);
		CREATE INDEX idx_agent_messages_parent ON agent_messages(parent_id);
		INSERT INTO agent_groups (id, name) VALUES (1, 'alpha'), (2, 'beta');
		INSERT INTO agent_messages
			(id, group_id, from_conv, to_conv, subject, body, created_at, parent_id) VALUES
			(1, 1, 'conv-a', 'conv-b', 'hi', 'first message',  '2020-01-01T00:00:00Z', 0),
			(2, 2, 'conv-c', 'conv-a', 're', 'second message', '2020-01-02T00:00:00Z', 1);
	`)
	require.NoError(t, err, "seed v35 schema")

	require.NoError(t, migrateV35toV36(d), "migrateV35toV36")

	// schema_version bumped to 36.
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 36, ver, "schema_version after migration")

	// The foreign key to agent_groups is gone.
	fkRows, err := d.Query(`PRAGMA foreign_key_list(agent_messages)`)
	require.NoError(t, err, "PRAGMA foreign_key_list")
	hasFK := fkRows.Next()
	require.NoError(t, fkRows.Close())
	assert.False(t, hasFK, "agent_messages should carry no foreign key after v36")

	// group_id keeps NOT NULL but gains a DEFAULT 0.
	cols := tableColumns(t, d, "agent_messages")
	assert.Subset(t, cols, []string{
		"id", "group_id", "from_conv", "to_conv", "subject", "body",
		"created_at", "delivered_at", "read_at", "parent_id",
		"to_recipients", "cc_recipients", "original_to_conv",
	}, "all columns survive the rebuild; got=%v", cols)

	// Every message row survived with group_id and the rest intact.
	type msg struct {
		gid              int64
		from, to, subj, body string
	}
	byID := map[int64]msg{}
	rows, err := d.Query(`SELECT id, group_id, from_conv, to_conv, subject, body FROM agent_messages`)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		var m msg
		require.NoError(t, rows.Scan(&id, &m.gid, &m.from, &m.to, &m.subj, &m.body))
		byID[id] = m
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Len(t, byID, 2, "both message rows preserved")
	assert.Equal(t, msg{1, "conv-a", "conv-b", "hi", "first message"}, byID[1])
	assert.Equal(t, msg{2, "conv-c", "conv-a", "re", "second message"}, byID[2])

	// The point of the migration: a direct message (group_id 0) inserts
	// cleanly. Pre-v36 the foreign key rejected this — 0 is not a real
	// agent_groups.id.
	_, err = d.Exec(`INSERT INTO agent_messages
		(group_id, from_conv, to_conv, created_at) VALUES
		(0, 'solo-x', 'solo-y', '2020-02-01T00:00:00Z')`)
	assert.NoError(t, err, "a group_id 0 (direct) message must insert cleanly post-v36")
}

// TestDeleteAgentGroup_PreservesMessagesAsDirect pins the universal-
// inbox data-retention rule: deleting a group does NOT destroy its
// message history. The messages survive, rewritten to group_id 0 —
// i.e. they become direct messages.
func TestDeleteAgentGroup_PreservesMessagesAsDirect(t *testing.T) {
	setupTestDB(t)

	gid, err := CreateAgentGroup("doomed", "")
	require.NoError(t, err, "CreateAgentGroup")

	keptID, err := InsertAgentMessage(&AgentMessage{
		GroupID:   gid,
		FromConv:  "conv-a",
		ToConv:    "conv-b",
		Body:      "remember me after the group is gone",
		CreatedAt: time.Now(),
	})
	require.NoError(t, err, "InsertAgentMessage")

	require.NoError(t, DeleteAgentGroup("doomed"), "DeleteAgentGroup")

	// The group itself is gone.
	g, err := GetAgentGroupByName("doomed")
	require.NoError(t, err)
	assert.Nil(t, g, "group should be deleted")

	// The message survives — rewritten to a direct message.
	m, err := GetAgentMessage(keptID)
	require.NoError(t, err, "GetAgentMessage")
	require.NotNil(t, m, "message must survive its group's deletion")
	assert.Equal(t, int64(0), m.GroupID,
		"a deleted group's messages become direct (group_id 0)")
	assert.Equal(t, "remember me after the group is gone", m.Body,
		"message body untouched")
}
