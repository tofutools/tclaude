package db

import "database/sql"

func migrateV244toV245(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE federation_teleport_leases (
 direction TEXT NOT NULL, peer TEXT NOT NULL, offer TEXT NOT NULL,
 source_agent TEXT NOT NULL, target_agent TEXT NOT NULL DEFAULT '',
 epoch INTEGER NOT NULL, state TEXT NOT NULL, revision INTEGER NOT NULL, snapshot TEXT NOT NULL,
 PRIMARY KEY(direction,peer,offer)) STRICT;
 CREATE UNIQUE INDEX federation_teleport_backup_source ON federation_teleport_leases(source_agent)
 WHERE direction='out' AND state NOT IN ('recovered','released');
 CREATE INDEX federation_teleport_leases_target ON federation_teleport_leases(target_agent);
 UPDATE schema_version SET version=245;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
