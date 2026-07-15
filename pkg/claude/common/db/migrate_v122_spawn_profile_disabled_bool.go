package db

import (
	"database/sql"
	"fmt"
)

// migrateV121toV122 separates the spawn-profile disable switch from its
// explanatory text. Rows disabled under v121 are backfilled to disabled=1;
// later enables can retain disabled_reason for reuse without blocking spawns.
func migrateV121toV122(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v121→v122 (explicit spawn-profile disabled state): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'disabled'`).Scan(&haveColumn); err != nil {
		return fmt.Errorf("migrate v121→v122 (explicit spawn-profile disabled state): probe column: %w", err)
	}
	if haveColumn == 0 {
		if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v121→v122 (explicit spawn-profile disabled state): add column: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE spawn_profiles SET disabled = 1 WHERE disabled_reason <> ''`); err != nil {
		return fmt.Errorf("migrate v121→v122 (explicit spawn-profile disabled state): backfill: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 122`); err != nil {
		return fmt.Errorf("migrate v121→v122 (explicit spawn-profile disabled state): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v121→v122 (explicit spawn-profile disabled state): commit: %w", err)
	}
	return nil
}
