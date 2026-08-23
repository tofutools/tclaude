package db

import (
	"database/sql"
	"fmt"
)

// migrateV222toV223 preserves SSH-workaround intent while a spawn waits for
// its conversation id. NULL identifies legacy rows whose intent is unknown.
func migrateV222toV223(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v222→v223 (pending SSH workaround): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := addColumnIfMissing(tx, "pending_spawns", "ssh_workaround",
		`ALTER TABLE pending_spawns ADD COLUMN ssh_workaround INTEGER`); err != nil {
		return fmt.Errorf("migrate v222→v223 (pending SSH workaround): add column: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 223`); err != nil {
		return fmt.Errorf("migrate v222→v223 (pending SSH workaround): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v222→v223 (pending SSH workaround): commit: %w", err)
	}
	return nil
}
