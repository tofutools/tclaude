package db

import "database/sql"

// migrateV300toV301 lets a remote spawn request wait in the capacity spawn
// queue (tcl-ao1yz5). fed_request_id names the federation_spawn_requests row
// a queue entry holds a place for; local entries keep 0. The request itself
// stays in federation_spawn_requests with status "queued", so the entry has
// no request body.
func migrateV300toV301(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`ALTER TABLE spawn_queue ADD COLUMN fed_request_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 301`); err != nil {
		return err
	}
	return tx.Commit()
}
