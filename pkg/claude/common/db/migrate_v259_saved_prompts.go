package db

import "database/sql"

func migrateV258toV259(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS saved_prompts (
 name TEXT PRIMARY KEY NOT NULL,
 body TEXT NOT NULL
 );
 UPDATE schema_version SET version = 259;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
