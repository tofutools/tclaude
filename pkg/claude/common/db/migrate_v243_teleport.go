package db

import "database/sql"

func migrateV242toV243(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE federation_teleports (
 direction TEXT NOT NULL, peer TEXT NOT NULL, offer TEXT NOT NULL,
 chain TEXT NOT NULL, source_agent TEXT NOT NULL, target_agent TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, state TEXT NOT NULL, snapshot TEXT NOT NULL,
 PRIMARY KEY(direction,peer,offer)) STRICT;
 CREATE INDEX federation_teleports_rates ON federation_teleports(direction,source_agent,created_at);
 CREATE INDEX federation_teleports_peer_rates ON federation_teleports(direction,peer,created_at);
 CREATE INDEX federation_teleports_target ON federation_teleports(target_agent);
 UPDATE schema_version SET version=243;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
