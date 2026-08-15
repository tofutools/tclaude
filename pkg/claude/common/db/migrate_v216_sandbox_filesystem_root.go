package db

import (
	"database/sql"
	"fmt"
)

// migrateV215toV216 adds the explicit filesystem-root posture to sandbox
// profiles. The empty default preserves the prior automatic derivation.
func migrateV215toV216(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v215→v216 (sandbox filesystem root): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable, haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v215→v216 (sandbox filesystem root): probe table: %w", err)
	}
	if haveTable > 0 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'filesystem_root'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v215→v216 (sandbox filesystem root): probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN filesystem_root TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v215→v216 (sandbox filesystem root): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 216`); err != nil {
		return fmt.Errorf("migrate v215→v216 (sandbox filesystem root): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v215→v216 (sandbox filesystem root): commit: %w", err)
	}
	return nil
}
