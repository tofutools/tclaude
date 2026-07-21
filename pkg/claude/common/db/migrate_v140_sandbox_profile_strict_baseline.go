package db

import (
	"database/sql"
	"fmt"
)

// migrateV139toV140 adds the opt-in strict read baseline and the exceptional
// break-glass protected-path rules to sandbox profiles (TCL-609).
//
// Both defaults reproduce today's behavior exactly: an empty read_baseline
// inherits each harness's existing broad read posture, and an empty
// break_glass_filesystem_json carries no protected-path authority. Existing
// rows therefore keep their effective sandbox unchanged after the upgrade.
func migrateV139toV140(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v139→v140 (sandbox strict baseline): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v139→v140 (sandbox strict baseline): probe table: %w", err)
	}
	if haveTable > 0 {
		for _, add := range []struct{ column, ddl string }{
			{"read_baseline", `ALTER TABLE sandbox_profiles ADD COLUMN read_baseline TEXT NOT NULL DEFAULT ''`},
			{"break_glass_filesystem_json", `ALTER TABLE sandbox_profiles ADD COLUMN break_glass_filesystem_json TEXT NOT NULL DEFAULT '[]'`},
		} {
			var haveColumn int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = ?`, add.column).Scan(&haveColumn); err != nil {
				return fmt.Errorf("migrate v139→v140 (probe sandbox_profiles.%s): %w", add.column, err)
			}
			if haveColumn > 0 {
				continue
			}
			if _, err := tx.Exec(add.ddl); err != nil {
				return fmt.Errorf("migrate v139→v140 (add sandbox_profiles.%s): %w", add.column, err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 140`); err != nil {
		return fmt.Errorf("migrate v139→v140 (sandbox strict baseline): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v139→v140 (sandbox strict baseline): commit: %w", err)
	}
	return nil
}
