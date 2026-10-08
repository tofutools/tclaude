package db

import "database/sql"

func migrateV237toV238(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE federation_node_groups(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,created_at INTEGER NOT NULL);
 CREATE TABLE federation_node_group_members(group_id TEXT NOT NULL REFERENCES federation_node_groups(id) ON DELETE CASCADE,peer TEXT NOT NULL REFERENCES federation_peers(instance_id) ON DELETE CASCADE,PRIMARY KEY(group_id,peer));
 CREATE TABLE federation_node_group_grants(group_id TEXT NOT NULL REFERENCES federation_node_groups(id) ON DELETE CASCADE,slug TEXT NOT NULL,scope TEXT NOT NULL DEFAULT '',spawn_policy TEXT NOT NULL DEFAULT '{}',created_at INTEGER NOT NULL,PRIMARY KEY(group_id,slug,scope));
 UPDATE schema_version SET version=238;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
