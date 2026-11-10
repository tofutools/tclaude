package db

import "database/sql"

// migrateV292toV293 adds conv_loss_reports: the loss report of the conversion
// that produced a conversation, as losses.json with its prior chain. The
// conversation's own harness files have nowhere to keep it, and without it a
// later morph or export could only describe its own step.
func migrateV292toV293(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS conv_loss_reports (
		conv_id TEXT PRIMARY KEY,
		report  TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 293`); err != nil {
		return err
	}
	return tx.Commit()
}
