package db

import (
	"database/sql"
	"fmt"
)

// migrateV217toV218 adds the profile-side toggle that controls whether a new
// spawn worktree fetches its base branch before it is cut. NULL preserves the
// spawn dialog's default (on); 0 and 1 are explicit profile overrides.
func migrateV217toV218(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v217→v218 (add fetch_latest_worktree): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'spawn_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v217→v218 (add fetch_latest_worktree): probe table: %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'fetch_latest_worktree'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v217→v218 (add fetch_latest_worktree): probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN fetch_latest_worktree INTEGER`); err != nil {
				return fmt.Errorf("migrate v217→v218 (add fetch_latest_worktree): add column: %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 218`); err != nil {
		return fmt.Errorf("migrate v217→v218 (add fetch_latest_worktree): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v217→v218 (add fetch_latest_worktree): commit: %w", err)
	}
	return nil
}
