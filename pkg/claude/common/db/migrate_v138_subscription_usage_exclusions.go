package db

import (
	"database/sql"
	"fmt"
)

// migrateV137toV138 lets the operator exclude individual quota observations
// without deleting the underlying history. Keeping the flag on the window row
// makes it follow the same retention and cascade rules as the observation.
func migrateV137toV138(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v137→v138: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'subscription_usage_windows'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v137→v138: probe table: %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('subscription_usage_windows')
			WHERE name = 'excluded'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v137→v138: probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE subscription_usage_windows
				ADD COLUMN excluded INTEGER NOT NULL DEFAULT 0 CHECK(excluded IN (0, 1))`); err != nil {
				return fmt.Errorf("migrate v137→v138: add excluded: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 138`); err != nil {
		return fmt.Errorf("migrate v137→v138: version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v137→v138: commit: %w", err)
	}
	return nil
}
