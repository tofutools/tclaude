package db

import "database/sql"

// migrateV257toV258 adds agent_messages_fts, a full-text index over agent
// message subjects and bodies for the Messages-tab search box. A LIKE
// '%q%' over every body is a full read of the message text — ~2s at 100k
// messages, re-run on each 2s refresh while a search is active.
//
// It is an external-content FTS5 table (the text lives only in
// agent_messages), kept in step by triggers: rows are indexed on insert,
// dropped on delete, and re-indexed only when subject or body change, so the
// frequent read/delivery-state updates never touch it. The rebuild backfills
// existing rows.
//
// The table exists on every real database; the guard only spares partial
// fixtures that carry an old schema without it.
func migrateV257toV258(d *sql.DB) error {
	stmts := `
CREATE VIRTUAL TABLE IF NOT EXISTS agent_messages_fts USING fts5(
	subject, body, content='agent_messages', content_rowid='id',
	tokenize='unicode61 remove_diacritics 2');
CREATE TRIGGER IF NOT EXISTS agent_messages_fts_ai AFTER INSERT ON agent_messages BEGIN
	INSERT INTO agent_messages_fts(rowid, subject, body) VALUES (new.id, new.subject, new.body);
END;
CREATE TRIGGER IF NOT EXISTS agent_messages_fts_ad AFTER DELETE ON agent_messages BEGIN
	INSERT INTO agent_messages_fts(agent_messages_fts, rowid, subject, body)
		VALUES ('delete', old.id, old.subject, old.body);
END;
CREATE TRIGGER IF NOT EXISTS agent_messages_fts_au AFTER UPDATE OF subject, body ON agent_messages BEGIN
	INSERT INTO agent_messages_fts(agent_messages_fts, rowid, subject, body)
		VALUES ('delete', old.id, old.subject, old.body);
	INSERT INTO agent_messages_fts(rowid, subject, body) VALUES (new.id, new.subject, new.body);
END;
INSERT INTO agent_messages_fts(agent_messages_fts) VALUES ('rebuild');`
	if ok, err := tableExists(d, "agent_messages"); err != nil {
		return err
	} else if !ok {
		stmts = ""
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(stmts + `
UPDATE schema_version SET version=258;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
