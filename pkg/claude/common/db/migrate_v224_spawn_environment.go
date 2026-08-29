package db

import (
	"database/sql"
	"fmt"
)

func migrateV223toV224(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v223→v224 (spawn environment): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"agent_groups", "spawn_profiles"} {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&exists); err != nil {
			return fmt.Errorf("migrate v223→v224 (spawn environment): probe %s: %w", table, err)
		}
		if exists == 0 {
			continue
		}
		if err := addColumnIfMissing(tx, table, "environment_json",
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN environment_json TEXT NOT NULL DEFAULT '[]'", table)); err != nil {
			return fmt.Errorf("migrate v223→v224 (spawn environment): %s: %w", table, err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 224`); err != nil {
		return fmt.Errorf("migrate v223→v224 (spawn environment): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v223→v224 (spawn environment): commit: %w", err)
	}
	return nil
}
