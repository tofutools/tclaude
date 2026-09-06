package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestOpenPreservesReplacementDataWhileScopingRequestIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replacement-old.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`
CREATE TABLE backend_meta(singleton INTEGER PRIMARY KEY, revision INTEGER NOT NULL);
INSERT INTO backend_meta VALUES(1,7);
CREATE TABLE agents(id TEXT PRIMARY KEY,name TEXT NOT NULL,harness TEXT NOT NULL,model TEXT NOT NULL,working_directory TEXT NOT NULL,approval TEXT NOT NULL,sandbox TEXT NOT NULL,primary_execution_id TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
INSERT INTO agents VALUES('agent_old','Old','fake','test','/tmp','supervised','unconfined','',1,1,1);
CREATE TABLE groups(id TEXT PRIMARY KEY,name TEXT NOT NULL,revision INTEGER NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
INSERT INTO groups VALUES('group_old','Old group',1,1,1);
CREATE TABLE group_members(group_id TEXT NOT NULL REFERENCES groups(id),agent_id TEXT NOT NULL REFERENCES agents(id),position INTEGER NOT NULL,PRIMARY KEY(group_id,agent_id));
INSERT INTO group_members VALUES('group_old','agent_old',0);
CREATE TABLE conversations(id TEXT PRIMARY KEY,revision INTEGER NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE agent_conversations(agent_id TEXT NOT NULL REFERENCES agents(id),conversation_id TEXT NOT NULL REFERENCES conversations(id),current INTEGER NOT NULL,revision INTEGER NOT NULL,associated_at INTEGER NOT NULL,replaced_at INTEGER,PRIMARY KEY(agent_id,conversation_id));
CREATE TABLE executions(id TEXT PRIMARY KEY,agent_id TEXT NOT NULL DEFAULT '',conversation_id TEXT NOT NULL,harness TEXT NOT NULL,model TEXT NOT NULL,working_directory TEXT NOT NULL,approval TEXT NOT NULL,sandbox TEXT NOT NULL,state TEXT NOT NULL,evidence_provider TEXT NOT NULL DEFAULT '',evidence_version INTEGER NOT NULL DEFAULT 0,evidence_payload BLOB,native_namespace TEXT NOT NULL DEFAULT '',native_reference TEXT NOT NULL DEFAULT '',native_observed_at INTEGER,revision INTEGER NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE operations(id TEXT PRIMARY KEY,request_id TEXT NOT NULL UNIQUE,kind TEXT NOT NULL,principal_kind TEXT NOT NULL,principal_agent_id TEXT NOT NULL DEFAULT '',execution_id TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,result_code TEXT NOT NULL DEFAULT '',detail TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
INSERT INTO operations VALUES('op_old','request_old','send_message','operator','','','succeeded','committed','',1,1,1);
CREATE TABLE release_permits(execution_id TEXT PRIMARY KEY REFERENCES executions(id),operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id),consumed_at INTEGER);
CREATE TABLE messages(id TEXT PRIMARY KEY,operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id),sender_kind TEXT NOT NULL,sender_agent_id TEXT NOT NULL DEFAULT '',body TEXT NOT NULL,created_at INTEGER NOT NULL);
INSERT INTO messages VALUES('message_old','op_old','operator','','preserved',1);
CREATE TABLE message_recipients(id TEXT PRIMARY KEY,message_id TEXT NOT NULL REFERENCES messages(id),agent_id TEXT NOT NULL REFERENCES agents(id),read_at INTEGER,notified INTEGER NOT NULL DEFAULT 0,UNIQUE(message_id,agent_id));
INSERT INTO message_recipients VALUES('recipient_old','message_old','agent_old',NULL,0);
`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	store, err := Open(path)
	require.NoError(t, err)
	defer store.Close()
	snapshot, err := store.Snapshot(context.Background())
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Len(t, snapshot.Groups, 1)
	require.Len(t, snapshot.Operations, 1)
	require.Len(t, snapshot.Messages, 1)
	require.Equal(t, "preserved", snapshot.Messages[0].Body)

	_, err = store.db.Exec(`INSERT INTO operations(id,request_id,request_scope,kind,principal_kind,state,revision,created_at,updated_at) VALUES('op_new','request_old','execution:exe_new','interact','execution','succeeded',1,2,2)`)
	require.NoError(t, err, "the same client request id is isolated by authenticated execution scope")
	rows, err := store.db.Query(`PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer rows.Close()
	require.False(t, rows.Next(), "schema evolution must preserve valid foreign keys")
}
