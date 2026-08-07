package db

import (
	"database/sql"
	"fmt"
)

// migrateV189toV190 adds the optional Copilot context-window launch field to
// spawn profiles. The durable per-conversation value lives in the versioned
// relaunch-profile JSON, so no sessions column is needed.
func migrateV189toV190(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v189→v190 (Copilot context window): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var have int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'context_window_max'`).Scan(&have); err != nil {
		return fmt.Errorf("migrate v189→v190 (Copilot context window): probe: %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN context_window_max INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v189→v190 (Copilot context window): add column: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 190`); err != nil {
		return fmt.Errorf("migrate v189→v190 (Copilot context window): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v189→v190 (Copilot context window): commit: %w", err)
	}
	return nil
}
