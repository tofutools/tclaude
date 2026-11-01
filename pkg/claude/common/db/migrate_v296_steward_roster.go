package db

import "database/sql"

// migrateV295toV296 adds the Steward roster: the one active Steward, chosen by
// the operator, and the former Stewards on standby. It is keyed on the stable
// agent id, so a seat survives reincarnation. Nothing is seeded: until the
// operator picks, the newest running Steward stays the active one.
func migrateV295toV296(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS steward_roster (
		agent_id TEXT NOT NULL PRIMARY KEY,
		seat     TEXT NOT NULL CHECK (seat IN ('active', 'standby')),
		since    INTEGER NOT NULL,
		set_by   TEXT NOT NULL
	) STRICT`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS steward_roster_one_active
		ON steward_roster (seat) WHERE seat = 'active'`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 296`); err != nil {
		return err
	}
	return tx.Commit()
}
