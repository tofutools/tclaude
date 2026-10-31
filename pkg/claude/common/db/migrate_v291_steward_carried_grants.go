package db

import "database/sql"

// migrateV290toV291 adds what an operator chose to hand to all future
// Stewards. These tables belong to no agent, so retiring a Steward leaves them
// alone, and a Steward only ever reads them when it is summoned.
//
// agent_sudo_grants.carried_id ties an instance's sudo row to the stored grant
// it came from, so revoking or extending either reaches the other.
func migrateV290toV291(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS steward_carried_permissions (
 slug TEXT PRIMARY KEY,
 effect TEXT NOT NULL CHECK(effect IN ('grant', 'deny')),
 scope_json TEXT NOT NULL DEFAULT '',
 granted_by TEXT NOT NULL,
 granted_at INTEGER NOT NULL
) STRICT`,
		`CREATE TABLE IF NOT EXISTS steward_carried_sudo (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 slug TEXT NOT NULL,
 scope_json TEXT NOT NULL DEFAULT '',
 granted_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL,
 granted_by TEXT NOT NULL,
 reason TEXT NOT NULL DEFAULT '',
 revoked_at INTEGER
) STRICT`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	// Some partial test schemas never had the table.
	var sudoTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_sudo_grants'`).Scan(&sudoTable); err != nil {
		return err
	}
	if sudoTable > 0 {
		if _, err := tx.Exec(`ALTER TABLE agent_sudo_grants ADD COLUMN carried_id INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 291`); err != nil {
		return err
	}
	return tx.Commit()
}
