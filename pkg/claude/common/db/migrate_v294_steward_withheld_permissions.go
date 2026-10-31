package db

import "database/sql"

// migrateV293toV294 records the permissions the operator took off a Steward, so
// the access its levels give can be brought up to date without handing one
// back. Removing a grant leaves no row, so past CLI revokes are recovered from
// the audit log, as far back as it reaches.
func migrateV293toV294(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS steward_withheld_permissions (
		agent_id TEXT NOT NULL,
		slug     TEXT NOT NULL,
		PRIMARY KEY (agent_id, slug)
	)`); err != nil {
		return err
	}
	// Some partial test schemas never had these tables.
	var found int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('audit_log', 'agents')`).Scan(&found); err != nil {
		return err
	}
	if found == 2 {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO steward_withheld_permissions (agent_id, slug)
			SELECT a.agent_id, l.detail FROM audit_log l JOIN agents a ON a.current_conv_id = l.target_conv
			WHERE l.verb = 'permissions.revoke' AND l.status < 400 AND l.detail <> ''`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 294`); err != nil {
		return err
	}
	return tx.Commit()
}
