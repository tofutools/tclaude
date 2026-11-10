package db

import "database/sql"

// migrateV301toV302 drops agent_turn_replies (tcl-t1ghd7). v263 added it for
// the scribe chat endpoint, which #3747 removed along with the Stop hook's
// write to it, so the table is neither written nor read. Its rows were scribe
// final messages, kept only for that chat. The v263 migration stays as
// history; dropping the table drops its index with it.
func migrateV301toV302(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DROP TABLE IF EXISTS agent_turn_replies`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 302`); err != nil {
		return err
	}
	return tx.Commit()
}
