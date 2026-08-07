package db

import (
	"database/sql"
	"fmt"
)

// migrateV190toV191 adds the optional Copilot API-backed-mode launch field to
// spawn profiles. Nullable, so an existing profile stays SILENT on the axis and
// keeps falling through to the send-keys default — the same tri-state shape as
// auto_memory / ssh_workaround. The durable per-conversation value lives in the
// versioned relaunch-profile JSON, so no sessions column is needed.
func migrateV190toV191(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v190→v191 (Copilot API mode): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var have int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'copilot_api'`).Scan(&have); err != nil {
		return fmt.Errorf("migrate v190→v191 (Copilot API mode): probe: %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN copilot_api INTEGER`); err != nil {
			return fmt.Errorf("migrate v190→v191 (Copilot API mode): add column: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 191`); err != nil {
		return fmt.Errorf("migrate v190→v191 (Copilot API mode): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v190→v191 (Copilot API mode): commit: %w", err)
	}
	return nil
}
