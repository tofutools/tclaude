package db

import "database/sql"

func migrateV238toV239(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE federation_agent_moves (
 direction TEXT NOT NULL, peer TEXT NOT NULL, id TEXT NOT NULL,
 source_agent TEXT NOT NULL, state TEXT NOT NULL, payload TEXT NOT NULL,
 PRIMARY KEY(direction,peer,id));
 CREATE UNIQUE INDEX federation_agent_moves_active_source ON federation_agent_moves(source_agent)
 WHERE direction='out' AND state IN ('awaiting_confirmation','confirmed','retiring','blocked');
 UPDATE schema_version SET version=239;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
