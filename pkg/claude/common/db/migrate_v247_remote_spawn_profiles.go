package db

import "database/sql"

func migrateV246toV247(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`ALTER TABLE federation_spawn_requests ADD COLUMN requested_profile TEXT NOT NULL DEFAULT '';
UPDATE schema_version SET version=247;`); err != nil {
		return err
	}
	return tx.Commit()
}
