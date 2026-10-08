package db

import "database/sql"

// Pin receiving groups by identity so renaming cannot orphan an offer and
// deleting/recreating a same-name group cannot retarget imported agents.
func migrateV235toV236(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`ALTER TABLE federation_bundle_offers ADD COLUMN group_id INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE federation_bundle_offers ADD COLUMN sender_agent TEXT NOT NULL DEFAULT '';
 ALTER TABLE federation_bundle_offers ADD COLUMN import_agent TEXT NOT NULL DEFAULT '';
 ALTER TABLE federation_bundle_offers ADD COLUMN import_label TEXT NOT NULL DEFAULT '';
 UPDATE schema_version SET version=236;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
