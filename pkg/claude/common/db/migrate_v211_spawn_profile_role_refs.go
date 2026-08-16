package db

import (
	"database/sql"
	"fmt"
)

// migrateV210toV211 expands a spawn profile's single role preset into an
// ordered set. role_ref remains as a compatibility projection of the first
// role for older clients; role_refs is the canonical complete set.
func migrateV210toV211(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v210→v211 (spawn profile role refs): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable, haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'spawn_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v210→v211 (spawn profile role refs): table probe: %w", err)
	}
	if haveTable > 0 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'role_refs'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v210→v211 (spawn profile role refs): column probe: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN role_refs TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return fmt.Errorf("migrate v210→v211 (spawn profile role refs): add column: %w", err)
			}
		}
		if _, err := tx.Exec(`UPDATE spawn_profiles SET role_refs = json_array(role_ref) WHERE role_ref <> '' AND role_refs = '[]'`); err != nil {
			return fmt.Errorf("migrate v210→v211 (spawn profile role refs): backfill: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 211`); err != nil {
		return fmt.Errorf("migrate v210→v211 (spawn profile role refs): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v210→v211 (spawn profile role refs): commit: %w", err)
	}
	return nil
}
