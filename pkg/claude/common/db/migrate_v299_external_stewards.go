package db

import "database/sql"

// migrateV298toV299 adds external stewards: an assistant the operator runs
// outside tclaude, named here and proved by a credential. A principal is an
// ordinary agents row, so its permissions, inbox and audit are the agent ones;
// these tables hold only what is its own. Only a hash of a credential is stored.
func migrateV298toV299(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS external_stewards (
		agent_id   TEXT NOT NULL PRIMARY KEY REFERENCES agents(agent_id) ON DELETE CASCADE,
		name       TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL,
		created_by TEXT NOT NULL
	) STRICT`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS external_steward_credentials (
		token_id     TEXT NOT NULL PRIMARY KEY,
		agent_id     TEXT NOT NULL REFERENCES external_stewards(agent_id) ON DELETE CASCADE,
		token_hash   BLOB NOT NULL,
		created_at   INTEGER NOT NULL,
		created_by   TEXT NOT NULL,
		expires_at   INTEGER NOT NULL,
		revoked_at   INTEGER,
		last_used_at INTEGER
	) STRICT`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_external_steward_credentials_agent
		ON external_steward_credentials (agent_id)`); err != nil {
		return err
	}
	// A trigger because several writers retire an agent. Bringing the agent back
	// does not bring its credentials back.
	if _, err := tx.Exec(`CREATE TRIGGER IF NOT EXISTS external_steward_retired
		AFTER UPDATE OF retired_at ON agents WHEN NEW.retired_at IS NOT NULL BEGIN
			UPDATE external_steward_credentials SET revoked_at = NEW.retired_at
			WHERE agent_id = NEW.agent_id AND revoked_at IS NULL;
		END`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 299`); err != nil {
		return err
	}
	return tx.Commit()
}
