package db

import (
	"database/sql"
	"fmt"
)

// migrateV221toV222 keeps the authoritative OpenCode server's immutable
// execution evidence beside its private runtime recovery record.
func migrateV221toV222(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v221→v222 (OpenCode execution boundary): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := addColumnIfMissing(tx, "opencode_runtimes", "execution_boundary_json",
		`ALTER TABLE opencode_runtimes ADD COLUMN execution_boundary_json TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrate v221→v222 (OpenCode execution boundary): add column: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 222`); err != nil {
		return fmt.Errorf("migrate v221→v222 (OpenCode execution boundary): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v221→v222 (OpenCode execution boundary): commit: %w", err)
	}
	return nil
}
