package db

import (
	"database/sql"
	"fmt"
)

// migrateV143toV144 drops the read_baseline and read_baseline_exclusions_json
// columns added in v140/v142. TCL-623 replaced that separate strict-read
// mechanism with ordinary filesystem rows (a broad deny plus narrower reopens),
// so the columns have no reader left.
//
// Any value they carried is deliberately discarded rather than translated: the
// operator decision was to drop the feature outright ("nobody is using this
// yet"), and inventing deny rows on an operator's behalf would silently change
// what their profiles enforce. A profile that wants the old posture re-authors
// it as visible, editable rows.
//
// DROP COLUMN needs SQLite 3.35+; the probes keep the migration idempotent and
// a no-op on installs that never had the columns.
func migrateV143toV144(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v143→v144 (drop sandbox read baseline): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sandbox_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v143→v144 (drop sandbox read baseline): probe table: %w", err)
	}
	if haveTable > 0 {
		for _, column := range []string{"read_baseline", "read_baseline_exclusions_json"} {
			var haveColumn int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = ?`, column).Scan(&haveColumn); err != nil {
				return fmt.Errorf("migrate v143→v144 (probe sandbox_profiles.%s): %w", column, err)
			}
			if haveColumn == 0 {
				continue
			}
			if _, err := tx.Exec(`ALTER TABLE sandbox_profiles DROP COLUMN ` + column); err != nil {
				return fmt.Errorf("migrate v143→v144 (drop sandbox_profiles.%s): %w", column, err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 144`); err != nil {
		return fmt.Errorf("migrate v143→v144 (drop sandbox read baseline): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v143→v144 (drop sandbox read baseline): commit: %w", err)
	}
	return nil
}
