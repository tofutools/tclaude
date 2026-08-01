package db

import (
	"database/sql"
	"fmt"
)

func migrateV179toV180(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v179→v180 (OpenCode resource cgroup): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var have int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('opencode_runtimes') WHERE name = 'resource_cgroup_dir'`).Scan(&have); err != nil {
		return fmt.Errorf("migrate v179→v180 (probe resource cgroup): %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(`ALTER TABLE opencode_runtimes ADD COLUMN resource_cgroup_dir TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate v179→v180 (add resource cgroup): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 180`); err != nil {
		return fmt.Errorf("migrate v179→v180 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v179→v180 (commit): %w", err)
	}
	return nil
}
