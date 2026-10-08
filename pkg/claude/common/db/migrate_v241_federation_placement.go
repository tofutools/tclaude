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
`)
	if err != nil {
		return err
	}
	// Minimal recovery fixtures can omit this old table, as in prior pending
	// migrations. Normal databases always have it.
	var havePending int
	if err := tx.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='pending_spawns'").Scan(&havePending); err != nil {
		return err
	}
	if havePending != 0 {
		if _, err := tx.Exec("ALTER TABLE pending_spawns ADD COLUMN capacity_reserved INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE schema_version SET version=241"); err != nil {
		return err
	}
	return tx.Commit()
}
