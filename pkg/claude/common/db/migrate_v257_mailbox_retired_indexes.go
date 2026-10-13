package db

import "database/sql"

// migrateV256toV257 indexes agent_messages for the Messages tab's
// retired-agent exclude (MailboxFilter.ExcludeRetired), which the "all"
// firehose and every group badge apply by default:
//
//   - from_conv, to_conv, group_id, created_at replaces from_conv,
//     created_at: same leading column for the sender lookups, still covering
//     the sent-side tally, and it carries every column the exclude and the
//     roster's one-pass totals (MailboxRosterTotals) read, so that pass scans
//     this index instead of the message rows. Scanning in from_conv order
//     also keeps the per-row retired lookups on hot pages.
//   - a partial index of the reincarnation handoffs, the exclude's carve-out,
//     so testing it never reads message rows either.
//
// The table exists on every real database; the guard only spares partial
// fixtures that carry an old schema without it.
func migrateV256toV257(d *sql.DB) error {
	stmts := `
CREATE INDEX IF NOT EXISTS idx_agent_messages_parties ON agent_messages(from_conv, to_conv, group_id, created_at);
DROP INDEX IF EXISTS idx_agent_messages_from_conv;
CREATE INDEX IF NOT EXISTS idx_agent_messages_handoff ON agent_messages(id) WHERE subject = '` + ReincarnationHandoffSubject + `';`
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
UPDATE schema_version SET version=257;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
