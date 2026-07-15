package db

import (
	"database/sql"
	"fmt"
)

// migrateV123toV124 adds the optional sandbox-profile network posture. Empty
// preserves the pre-existing harness behavior; explicit values are validated
// at the sandbox-policy boundary before they are stored or launched.
func migrateV123toV124(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v123→v124 (sandbox network access): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable, haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v123→v124 (sandbox network access): probe table: %w", err)
	}
	if haveTable > 0 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'network_access'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v123→v124 (sandbox network access): probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN network_access TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v123→v124 (sandbox network access): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 124`); err != nil {
		return fmt.Errorf("migrate v123→v124 (sandbox network access): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v123→v124 (sandbox network access): commit: %w", err)
	}
	return nil
}
