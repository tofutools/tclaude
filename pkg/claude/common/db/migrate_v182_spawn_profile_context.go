package db

import (
	"database/sql"
	"fmt"
)

// migrateV181toV182 adds profile-authored startup guidance and persists the
// resolved value on pending spawns so delayed enrollment cannot lose it.
func migrateV181toV182(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v181→v182 (add spawn profile context): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, col := range []struct{ table, name string }{
		{"spawn_profiles", "startup_context"},
		{"pending_spawns", "profile_context"},
	} {
		var have int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, col.table, col.name).Scan(&have); err != nil {
			return fmt.Errorf("migrate v181→v182: probe %s.%s: %w", col.table, col.name, err)
		}
		if have == 0 {
			if _, err := tx.Exec(`ALTER TABLE ` + col.table + ` ADD COLUMN ` + col.name + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v181→v182: add %s.%s: %w", col.table, col.name, err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 182`); err != nil {
		return fmt.Errorf("migrate v181→v182: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v181→v182: commit: %w", err)
	}
	return nil
}
