package db

import (
	"database/sql"
	"fmt"
)

// migrateV208toV209 lets a spawn profile select a behavioral/access role
// independently of its free-text membership role label. The name reference is
// intentionally soft, like template role_ref: the API validates writes and the
// role delete guard prevents ordinary dangling references.
func migrateV208toV209(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v208→v209 (spawn profile role ref): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable, haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'spawn_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v208→v209 (spawn profile role ref): table probe: %w", err)
	}
	if haveTable > 0 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'role_ref'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v208→v209 (spawn profile role ref): column probe: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN role_ref TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v208→v209 (spawn profile role ref): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 209`); err != nil {
		return fmt.Errorf("migrate v208→v209 (spawn profile role ref): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v208→v209 (spawn profile role ref): commit: %w", err)
	}
	return nil
}
