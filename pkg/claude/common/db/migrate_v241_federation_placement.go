package db

import "database/sql"

func migrateV240toV241(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`ALTER TABLE federation_spawn_requests ADD COLUMN placement_version INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE federation_spawn_requests ADD COLUMN requirements TEXT NOT NULL DEFAULT '';
 UPDATE schema_version SET version=241;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
