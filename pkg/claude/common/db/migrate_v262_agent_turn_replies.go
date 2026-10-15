package db

import "database/sql"

// migrateV261toV262 adds agent_turn_replies: a bounded per-agent ring of the
// final assistant message of each turn, captured from the Stop hook for
// process scribes only (RecordScribeTurnReply). It is the read side of the
// process editor's docked scribe chat: the human's half is the operator
// message trail, the scribe's half is this table. Rows cascade with the agent.
func migrateV261toV262(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS agent_turn_replies (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	agent_id   TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
	conv_id    TEXT NOT NULL,
	body       TEXT NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_agent_turn_replies_agent ON agent_turn_replies(agent_id, id);
UPDATE schema_version SET version = 262;`); err != nil {
		return err
	}
	return tx.Commit()
}
