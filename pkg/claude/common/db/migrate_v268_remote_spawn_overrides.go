package db

import (
	"database/sql"
	"fmt"
)

// migrateV267toV268 stores the launch settings (cwd, harness, model, effort)
// a peer chose on a remote spawn request, as JSON; empty means none.
func migrateV267toV268(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v267→v268 (remote spawn overrides): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`ALTER TABLE federation_spawn_requests ADD COLUMN launch_overrides TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrate v267→v268 (add launch_overrides): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 268`); err != nil {
		return fmt.Errorf("migrate v267→v268 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v267→v268 (commit): %w", err)
	}
	return nil
}
