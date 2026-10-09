package db

import "database/sql"

const identityRotationSchema = `CREATE TABLE federation_identity_rotations (
 old_instance TEXT PRIMARY KEY,new_instance TEXT NOT NULL,statement TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','accepted','conflict','revoked','recovered')),
 received_at INTEGER NOT NULL,accept_after INTEGER NOT NULL,reason TEXT NOT NULL DEFAULT ''
) STRICT;`

func migrateV249toV250(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(identityRotationSchema + `UPDATE schema_version SET version=250;`); err != nil {
		return err
	}
	return tx.Commit()
}
