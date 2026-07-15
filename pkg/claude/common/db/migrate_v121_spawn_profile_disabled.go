package db

import (
	"database/sql"
	"fmt"
)

// migrateV120toV121 adds the durable disable switch for spawn profiles. The
// reason is the switch: empty means enabled, while a non-empty value both
// blocks use and explains the block to operators.
func migrateV120toV121(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v120→v121 (disable spawn profiles): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'disabled_reason'`).Scan(&haveColumn); err != nil {
		return fmt.Errorf("migrate v120→v121 (disable spawn profiles): probe column: %w", err)
	}
	if haveColumn == 0 {
		if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN disabled_reason TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate v120→v121 (disable spawn profiles): add column: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 121`); err != nil {
		return fmt.Errorf("migrate v120→v121 (disable spawn profiles): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v120→v121 (disable spawn profiles): commit: %w", err)
	}
	return nil
}
