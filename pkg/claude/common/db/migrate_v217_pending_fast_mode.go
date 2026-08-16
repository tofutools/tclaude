package db

import (
	"database/sql"
	"fmt"
)

// migrateV216toV217 preserves the effective inherited Fast-mode observation
// while a Codex spawn waits for its conversation id. NULL means legacy or
// unknown; both 0 and 1 are authoritative launch-time observations.
func migrateV216toV217(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v216→v217 (pending Fast mode): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'pending_spawns'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v216→v217 (pending Fast mode): probe table: %w", err)
	}
	if haveTable != 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pending_spawns') WHERE name = 'fast_mode_at_launch'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v216→v217 (pending Fast mode): probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE pending_spawns ADD COLUMN fast_mode_at_launch INTEGER`); err != nil {
				return fmt.Errorf("migrate v216→v217 (pending Fast mode): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 217`); err != nil {
		return fmt.Errorf("migrate v216→v217 (pending Fast mode): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v216→v217 (pending Fast mode): commit: %w", err)
	}
	return nil
}
