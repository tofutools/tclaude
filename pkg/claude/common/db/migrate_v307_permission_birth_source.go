package db

import (
	"database/sql"
	"fmt"
)

// migrateV306toV307 adds `agent_permissions.birth_source`: which spawn profile
// (and role) a birth-time grant or deny came from, as JSON (see
// PermissionBirthSource). Re-applying an edited spawn profile may change only
// the rows that profile wrote at birth and nobody has changed since, so every
// later write of a row resets it to ”.
//
// Additive and idempotent like v305: the column is probed, and NOT NULL
// DEFAULT ” marks every existing row as "source not recorded", which a
// re-apply treats as not the profile's. Nothing rewritten, no backup.
func migrateV306toV307(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v306→v307: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agent_permissions")
	if err != nil {
		return fmt.Errorf("migrate v306→v307 (probe agent_permissions): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_permissions') WHERE name = 'birth_source'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v306→v307 (probe agent_permissions.birth_source): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(`ALTER TABLE agent_permissions ADD COLUMN birth_source TEXT NOT NULL DEFAULT ''
				CHECK(length(CAST(birth_source AS BLOB)) BETWEEN 0 AND 4096)`); err != nil {
				return fmt.Errorf("migrate v306→v307 (add agent_permissions.birth_source): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 307`); err != nil {
		return fmt.Errorf("migrate v306→v307 (version): %w", err)
	}
	return tx.Commit()
}
