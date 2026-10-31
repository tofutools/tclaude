package db

import "database/sql"

// migrateV291toV292 adds a use limit to sudo grants. uses_limit 0 means no
// limit, and a grant with a limit ends when uses_left reaches 0 or its time
// runs out, whichever comes first. The stored copy keeps its own count, so a
// new Steward never refills one.
func migrateV291toV292(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"agent_sudo_grants", "steward_carried_sudo"} {
		// Some partial test schemas never had the table.
		var found int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			continue
		}
		for _, column := range []string{"uses_limit", "uses_left"} {
			if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 292`); err != nil {
		return err
	}
	return tx.Commit()
}
