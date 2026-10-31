package db

import "database/sql"

// migrateV292toV293 adds agent_paf_extensions: the copy: safe extension data
// of an imported PAF that tclaude does not understand, kept so a later export
// of the agent can write it back.
func migrateV292toV293(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS agent_paf_extensions (
		agent_id TEXT PRIMARY KEY REFERENCES agents(agent_id) ON DELETE CASCADE,
		data     TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 293`); err != nil {
		return err
	}
	return tx.Commit()
}
