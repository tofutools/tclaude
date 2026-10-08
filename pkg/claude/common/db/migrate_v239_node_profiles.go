package db

import "database/sql"

func migrateV238toV239(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
 CREATE TABLE federation_node_profiles(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,revision INTEGER NOT NULL,definition TEXT NOT NULL,created_at INTEGER NOT NULL) STRICT;
 CREATE TABLE federation_node_profile_default(singleton INTEGER PRIMARY KEY CHECK(singleton=1),profile_id TEXT NOT NULL REFERENCES federation_node_profiles(id)) STRICT;
 CREATE TABLE federation_node_profile_assignments(peer TEXT PRIMARY KEY REFERENCES federation_peers(instance_id) ON DELETE CASCADE,profile_id TEXT NOT NULL REFERENCES federation_node_profiles(id),snapshot TEXT NOT NULL) STRICT;
 CREATE TABLE federation_worker_defaults(agent_id TEXT PRIMARY KEY,snapshot TEXT NOT NULL) STRICT;
 UPDATE schema_version SET version=239;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
