package db

import (
	"database/sql"
	"fmt"
)

// migrateV111toV112 records the exact effective sandbox values used by each
// session generation. Actor snapshots drive identity-level lifecycle, while
// this per-launch copy is the immutable audit/restart record for what actually
// reached a specific pane.
func migrateV111toV112(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v111→v112 (session sandbox snapshot): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v111→v112 (probe sessions): %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'effective_sandbox_config'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v111→v112 (probe sessions.effective_sandbox_config): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN effective_sandbox_config TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v111→v112 (add sessions.effective_sandbox_config): %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 112`); err != nil {
		return fmt.Errorf("migrate v111→v112 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v111→v112 (commit): %w", err)
	}
	return nil
}
