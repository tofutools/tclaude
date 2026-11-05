package db

import "database/sql"

// migrateV313toV314 adds the receiving node's per-peer policy for peer views
// a peer's agents open (agent principals). No row means allow.
func migrateV313toV314(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS federation_peer_agent_views (
			peer TEXT PRIMARY KEY,
			policy TEXT NOT NULL CHECK (policy IN ('allow','deny')),
			updated_at TEXT NOT NULL
		)`,
		`UPDATE schema_version SET version = 314`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
