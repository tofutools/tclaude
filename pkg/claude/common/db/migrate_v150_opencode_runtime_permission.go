package db

import (
	"database/sql"
	"fmt"
)

// migrateV149toV150 persists the exact tclaude-generated OpenCode session
// permission rules beside the managed server record. OpenCode stores the same
// rules in its own session database, but agentd needs an independent source of
// truth to verify and reapply them after a serve restart.
func migrateV149toV150(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v149→v150 (OpenCode runtime permission): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'opencode_runtimes'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v149→v150 (OpenCode runtime permission): probe table: %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('opencode_runtimes') WHERE name = 'permission_json'`,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v149→v150 (OpenCode runtime permission): probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE opencode_runtimes ADD COLUMN permission_json TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v149→v150 (OpenCode runtime permission): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 150`); err != nil {
		return fmt.Errorf("migrate v149→v150 (OpenCode runtime permission): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v149→v150 (OpenCode runtime permission): commit: %w", err)
	}
	return nil
}
