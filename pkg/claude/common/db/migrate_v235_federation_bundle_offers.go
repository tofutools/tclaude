package db

import "database/sql"

func migrateV234toV235(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`CREATE TABLE federation_bundle_offers (
 id TEXT NOT NULL,
 peer TEXT NOT NULL,
 direction TEXT NOT NULL CHECK(direction IN ('in','out')),
 kind TEXT NOT NULL,
 descriptor TEXT NOT NULL,
 bytes INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','ready','applied','declined','expired')),
 last_error TEXT NOT NULL DEFAULT '',
 result_queued INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL,
 PRIMARY KEY(direction,peer,id)
 ) STRICT;
 CREATE INDEX idx_federation_bundle_offer_expiry ON federation_bundle_offers(state,expires_at);
 UPDATE schema_version SET version=235;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
