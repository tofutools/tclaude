package db

import (
	"database/sql"
	"fmt"
)

// migrateV112toV113 adds sandbox-profile composition: an ordered list of
// included profile names stored alongside the filesystem and environment
// payloads. Referential integrity (existence, acyclicity, depth) is enforced
// by the registry write paths rather than schema constraints, matching how
// the JSON payload columns are validated.
func migrateV112toV113(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v112→v113 (sandbox profile includes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'includes_json'`).Scan(&haveColumn); err != nil {
		return fmt.Errorf("migrate v112→v113 (probe sandbox_profiles.includes_json): %w", err)
	}
	if haveColumn == 0 {
		if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN includes_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return fmt.Errorf("migrate v112→v113 (add sandbox_profiles.includes_json): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 113`); err != nil {
		return fmt.Errorf("migrate v112→v113 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v112→v113 (commit): %w", err)
	}
	return nil
}
