package db

import "database/sql"

func migrateV233toV234(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`ALTER TABLE federation_peers ADD COLUMN trust_level TEXT NOT NULL DEFAULT 'restricted' CHECK(trust_level IN ('restricted', 'unrestricted'));
 UPDATE schema_version SET version=234;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
