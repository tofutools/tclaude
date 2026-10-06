package db

import "database/sql"

// Peer grants replace the unreleased federation export policy. Old exports
// confer no authority in the new model.
func migrateV232toV233(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
 DROP TABLE IF EXISTS federation_exports;
 DROP TABLE IF EXISTS federation_imports;
 CREATE TABLE federation_peer_grants (
 peer TEXT NOT NULL REFERENCES federation_peers(instance_id) ON DELETE CASCADE,
 slug TEXT NOT NULL,
 scope TEXT NOT NULL DEFAULT '',
 spawn_policy TEXT NOT NULL DEFAULT '{}',
 created_at INTEGER NOT NULL,
 PRIMARY KEY(peer, slug, scope)
 ) STRICT;
 CREATE TABLE federation_auto_workers (
 request_id INTEGER PRIMARY KEY REFERENCES federation_spawn_requests(id) ON DELETE CASCADE,
 peer TEXT NOT NULL,
 agent_id TEXT NOT NULL
 ) STRICT;
 ALTER TABLE federation_spawn_requests ADD COLUMN launch_label TEXT NOT NULL DEFAULT '';
 ALTER TABLE federation_spawn_requests ADD COLUMN launch_started_at INTEGER;
 ALTER TABLE federation_spawn_requests ADD COLUMN automatic INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE federation_spawn_requests ADD COLUMN notice_sent INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE federation_spawn_requests ADD COLUMN result_sent INTEGER NOT NULL DEFAULT 0;
 UPDATE schema_version SET version=233;
 `)
	if err != nil {
		return err
	}
	return tx.Commit()
}
