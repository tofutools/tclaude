package db

import "database/sql"

func migrateV250toV251(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`ALTER TABLE federation_peer_grants ADD COLUMN expires_at INTEGER;
 CREATE TABLE federation_peer_access_requests (
 id TEXT PRIMARY KEY, peer TEXT NOT NULL, slug TEXT NOT NULL, group_id INTEGER NOT NULL,
 grant_group_id INTEGER NOT NULL, grant_ttl_seconds INTEGER NOT NULL, expires_at INTEGER) STRICT;
 UPDATE schema_version SET version=251;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
