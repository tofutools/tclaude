package db

import "database/sql"

func migrateV258toV259(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS spend_alerts (
 period TEXT NOT NULL,
 period_start TEXT NOT NULL,
 threshold INTEGER NOT NULL,
 PRIMARY KEY (period, period_start, threshold)
 );
 UPDATE schema_version SET version=259;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
