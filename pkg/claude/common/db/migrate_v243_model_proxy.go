package db

import "database/sql"

func migrateV242toV243(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE model_proxy_launches (
 session TEXT NOT NULL, generation TEXT NOT NULL, reference TEXT NOT NULL,
 bearer_hash TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(session,generation)) STRICT;
 CREATE TABLE model_proxy_requests (
 id TEXT PRIMARY KEY, day TEXT NOT NULL, proxy TEXT NOT NULL, peer TEXT NOT NULL,
 session TEXT NOT NULL, model TEXT NOT NULL, charged_tokens INTEGER NOT NULL,
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0,
 status INTEGER NOT NULL DEFAULT 0, complete INTEGER NOT NULL DEFAULT 0,
 request_bytes INTEGER NOT NULL DEFAULT 0, response_bytes INTEGER NOT NULL DEFAULT 0,
 started_at INTEGER NOT NULL, duration_ms INTEGER NOT NULL DEFAULT 0) STRICT;
 CREATE INDEX model_proxy_daily ON model_proxy_requests(day,proxy,peer,session);
 ALTER TABLE spawn_profiles ADD COLUMN model_proxy TEXT NOT NULL DEFAULT '';
 UPDATE schema_version SET version=243;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
