package db

import "database/sql"

func migrateV239toV240(d *sql.DB) error {
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	_, e = tx.Exec(`
 CREATE TABLE federation_enroll_tokens(id TEXT PRIMARY KEY,public_token TEXT NOT NULL,secret_hash BLOB NOT NULL,profile_id TEXT NOT NULL,profile_revision INTEGER NOT NULL,max_uses INTEGER NOT NULL,used_count INTEGER NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL) STRICT;
 CREATE TABLE federation_enrollments(direction TEXT NOT NULL,token_id TEXT NOT NULL,peer TEXT NOT NULL,peer_key BLOB NOT NULL,local_key BLOB NOT NULL,public_token TEXT NOT NULL,retired INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,PRIMARY KEY(direction,token_id,peer)) STRICT;
 UPDATE schema_version SET version=240;`)
	if e != nil {
		return e
	}
	return tx.Commit()
}
