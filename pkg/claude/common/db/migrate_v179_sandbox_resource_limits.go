package db

import (
	"database/sql"
	"fmt"
)

// migrateV178toV179 adds the optional Linux cgroup-v2 resource budget payload.
// '{}' is the exact opt-out default: legacy profiles neither probe controllers
// nor change their launch path.
func migrateV178toV179(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v178→v179 (sandbox resource limits): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v178→v179 (probe sandbox_profiles): %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'resource_limits_json'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v178→v179 (probe resource limits): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN resource_limits_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
				return fmt.Errorf("migrate v178→v179 (add resource limits): %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 179`); err != nil {
		return fmt.Errorf("migrate v178→v179 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v178→v179 (commit): %w", err)
	}
	return nil
}
