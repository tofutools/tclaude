package db

import "database/sql"

func migrateV254toV255(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
CREATE TABLE managed_instruction_blobs (
 digest TEXT PRIMARY KEY, content BLOB NOT NULL
) STRICT;
CREATE TABLE managed_instructions (
 scope TEXT NOT NULL, group_id INTEGER REFERENCES agent_groups(id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('skill','rule')), name TEXT NOT NULL,
 digest TEXT NOT NULL REFERENCES managed_instruction_blobs(digest),
 PRIMARY KEY(scope,kind,name),
 CHECK((scope='user' AND group_id IS NULL) OR (group_id IS NOT NULL AND scope='group:'||group_id))
) STRICT;
CREATE TABLE managed_instruction_launches (
 agent_id TEXT PRIMARY KEY, body TEXT NOT NULL, resources TEXT NOT NULL,
 root TEXT NOT NULL DEFAULT ''
) STRICT;
UPDATE schema_version SET version=255;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
