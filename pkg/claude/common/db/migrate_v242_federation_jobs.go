package db

import "database/sql"

func migrateV241toV242(d *sql.DB) error {
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	_, e = tx.Exec(`CREATE TABLE federation_repos (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL,
 definition TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1);
 CREATE TABLE federation_jobs (
 id TEXT PRIMARY KEY, direction TEXT NOT NULL, peer TEXT NOT NULL,
 fingerprint TEXT NOT NULL, state TEXT NOT NULL, request TEXT NOT NULL,
 repo_id TEXT NOT NULL DEFAULT '', repo_revision INTEGER NOT NULL DEFAULT 0,
 worker_id TEXT NOT NULL DEFAULT '', caller_agent TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '{}',
 created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL) STRICT;
 CREATE INDEX federation_jobs_reservations ON federation_jobs(direction,state);
 UPDATE schema_version SET version=242;`)
	if e != nil {
		return e
	}
	return tx.Commit()
}
