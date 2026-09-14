package db

import "database/sql"

func migrateV229toV230(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN network_auto_sync INTEGER CHECK(network_auto_sync IN (0,1));
	ALTER TABLE sessions ADD COLUMN network_sync_id TEXT NOT NULL DEFAULT '';
	CREATE TABLE network_sync_launches (
	 id TEXT PRIMARY KEY, session_id TEXT NOT NULL, snapshot TEXT NOT NULL, launch_snapshot TEXT NOT NULL,
	 dependencies TEXT NOT NULL, requested TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 0,
	 acknowledged INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'starting',
	 detail TEXT NOT NULL DEFAULT '', heartbeat INTEGER NOT NULL);
	UPDATE schema_version SET version = 230;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
