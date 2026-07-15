package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestMigrateV122ToV123AgentMessageAttachments(t *testing.T) {
	d, err := sql.Open("sqlite", "file:migrate-v123?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Exec(`
		PRAGMA foreign_keys = ON;
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (122);
		CREATE TABLE agent_messages (id INTEGER PRIMARY KEY);
		INSERT INTO agent_messages (id) VALUES (7);
	`)
	require.NoError(t, err)
	require.NoError(t, migrateV122toV123(d))
	require.NoError(t, migrateV122toV123(d), "half-applied migration converges")
	_, err = d.Exec(`INSERT INTO agent_message_attachments
		(message_id, ordinal, filename, size_bytes, storage_path) VALUES (7, 0, 'note.txt', 4, '/tmp/note.txt')`)
	require.NoError(t, err)
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 123, version)
}

func TestAgentMessageAttachmentsRoundTripAndCascade(t *testing.T) {
	setupTestDB(t)
	id, err := InsertAgentMessageWithAttachments(&AgentMessage{
		ToConv: "recipient", Subject: "files", Body: "see attached", OperatorAuthored: true,
	}, []AgentMessageAttachment{{Filename: "note.txt", ContentType: "text/plain", SizeBytes: 4, StoragePath: "/tmp/note.txt"}})
	require.NoError(t, err)
	got, err := ListAgentMessageAttachments(id)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "note.txt", got[0].Filename)
	assert.Equal(t, "/tmp/note.txt", got[0].StoragePath)
	assert.True(t, IsOperatorAgentMessage(id))
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM agent_messages WHERE id = ?`, id)
	require.NoError(t, err)
	got, err = ListAgentMessageAttachments(id)
	require.NoError(t, err)
	assert.Empty(t, got, "message deletion cascades attachment metadata")
}
