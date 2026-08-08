package db

import (
	"database/sql"
	"fmt"
)

func migrateV193toV194(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v193→v194 (spawn profile fast mode): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var have int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'fast_mode'`).Scan(&have); err != nil {
		return fmt.Errorf("migrate v193→v194 (spawn profile fast mode): probe: %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN fast_mode INTEGER`); err != nil {
			return fmt.Errorf("migrate v193→v194 (spawn profile fast mode): add column: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 194`); err != nil {
		return fmt.Errorf("migrate v193→v194 (spawn profile fast mode): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v193→v194 (spawn profile fast mode): commit: %w", err)
	}
	return nil
}
