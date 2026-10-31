package db

import "database/sql"

// migrateV293toV294 records which permissions a Steward's levels have offered
// it, so a slug added to a level later can be told from one the operator took
// off: removing a grant leaves no row behind.
func migrateV293toV294(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS steward_offered_permissions (
		agent_id TEXT NOT NULL,
		slug     TEXT NOT NULL,
		PRIMARY KEY (agent_id, slug)
	) STRICT`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 294`); err != nil {
		return err
	}
	return tx.Commit()
}
