package db

import "database/sql"

func migrateV251toV252(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// No agent FK: terminal tombstones must survive an operator's full deletion.
	_, err = tx.Exec(`CREATE TABLE agent_federation_presence (
 agent_id TEXT PRIMARY KEY, home_instance TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('here','away','reserved','terminal')),
 current_instance TEXT NOT NULL, current_peer TEXT NOT NULL, predecessor_instance TEXT NOT NULL,
 departure_offer TEXT NOT NULL, arrival_offer TEXT NOT NULL, hop_count INTEGER NOT NULL, visit_epoch INTEGER NOT NULL,
 independent_clone TEXT NOT NULL DEFAULT '', arrival_rollback_json TEXT NOT NULL DEFAULT '', continuation_nonce_hash TEXT NOT NULL, transfer_json TEXT NOT NULL, departed_at INTEGER, arrived_at INTEGER, updated_at INTEGER NOT NULL
 ) STRICT; UPDATE schema_version SET version=252;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
