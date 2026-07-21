package db

import (
	"database/sql"
	"fmt"
)

// migrateV140toV141 stores semantic default-read exclusions. The empty JSON
// array preserves the exact pre-feature launch behavior for every existing
// profile.
func migrateV140toV141(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v140→v141 (sandbox read exclusions): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v140→v141 (sandbox read exclusions): probe table: %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'read_baseline_exclusions_json'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v140→v141 (probe sandbox_profiles.read_baseline_exclusions_json): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN read_baseline_exclusions_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return fmt.Errorf("migrate v140→v141 (add sandbox_profiles.read_baseline_exclusions_json): %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 141`); err != nil {
		return fmt.Errorf("migrate v140→v141 (sandbox read exclusions): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v140→v141 (sandbox read exclusions): commit: %w", err)
	}
	return nil
}
