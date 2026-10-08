package db

import "database/sql"

func migrateV245toV246(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE model_proxy_leases (
 id TEXT PRIMARY KEY, peer TEXT NOT NULL, request TEXT NOT NULL, kind TEXT NOT NULL,
 proxy TEXT NOT NULL, worker TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '',
 generation TEXT NOT NULL DEFAULT '', revoked INTEGER NOT NULL DEFAULT 0,
 idle_seconds INTEGER NOT NULL, touched_at INTEGER NOT NULL,
 UNIQUE(peer,request,kind)) STRICT;
 CREATE TABLE model_proxy_worker_leases (
 worker TEXT PRIMARY KEY, gateway TEXT NOT NULL, lease TEXT NOT NULL,
 request TEXT NOT NULL, kind TEXT NOT NULL, proxy TEXT NOT NULL) STRICT;
 ALTER TABLE model_proxy_launches ADD COLUMN lease TEXT NOT NULL DEFAULT '';
 ALTER TABLE federation_spawn_requests ADD COLUMN credentials TEXT NOT NULL DEFAULT '';
 ALTER TABLE federation_spawn_requests ADD COLUMN model_lease TEXT NOT NULL DEFAULT '';
 UPDATE schema_version SET version=246;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
