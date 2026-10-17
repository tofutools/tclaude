package db

import (
	"database/sql"
	"fmt"
)

// migrateV268toV269 stores the launch settings (cwd, harness, model, effort)
// a peer chose on a remote spawn request, as JSON; empty means none.
func migrateV268toV269(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v268→v269 (remote spawn overrides): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`ALTER TABLE federation_spawn_requests ADD COLUMN launch_overrides TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrate v268→v269 (add launch_overrides): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 269`); err != nil {
		return fmt.Errorf("migrate v268→v269 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v268→v269 (commit): %w", err)
	}
	return nil
}
