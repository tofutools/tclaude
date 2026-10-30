package db

import "database/sql"

// migrateV283toV284 adds held_context_reinjections: at most one per session,
// a compaction re-injection waiting to learn whose compaction it was. Claude
// Code fires a subagent's compaction hooks on the main conversation with no
// agent_id, so while a subagent runs the hook alone cannot tell. The row keeps
// the main transcript's size at that moment; a boundary appended past it
// proves the main thread compacted. See session.QueueContextReinjection.
func migrateV283toV284(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS held_context_reinjections (
		session_id        TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
		conv_id           TEXT NOT NULL,
		transcript_offset INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 284`); err != nil {
		return err
	}
	return tx.Commit()
}
