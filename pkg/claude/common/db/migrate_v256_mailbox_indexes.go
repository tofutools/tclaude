package db

import "database/sql"

// migrateV255toV256 indexes agent_messages for the dashboard Messages tab.
// Every folder scope there is an OR over sender/recipient/group —
// (to_conv = ? OR from_conv = ?) for an agent folder, from_conv IN (…) OR
// to_conv IN (…) OR group_id = ? for a group folder — and the sidebar roster
// tallies GROUP BY to_conv / from_conv. With only a to_conv index each of
// those was a full table scan, which drags every message body through the page
// cache; at ~20k messages the 2s roster refresh alone cost a second.
//
//   - from_conv, created_at: the sender half of the OR scopes (SQLite's
//     multi-index OR needs every arm indexed) and a covering index for the
//     roster's sent-side tally.
//   - group_id: the group-channel arm of a group folder's scope.
//   - to_conv, created_at, read_at replaces to_conv, created_at: same
//     leading columns, so every existing user keeps its plan, and the roster's
//     received-side tally (count, unread, newest) is answered from the index.
//
// The table exists on every real database; the guard only spares partial
// fixtures that carry an old schema without it.
func migrateV255toV256(d *sql.DB) error {
	stmts := `
CREATE INDEX IF NOT EXISTS idx_agent_messages_from_conv ON agent_messages(from_conv, created_at);
CREATE INDEX IF NOT EXISTS idx_agent_messages_group ON agent_messages(group_id);
CREATE INDEX IF NOT EXISTS idx_agent_messages_to_conv_read ON agent_messages(to_conv, created_at, read_at);
DROP INDEX IF EXISTS idx_agent_messages_to_conv;`
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
UPDATE schema_version SET version=256;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
