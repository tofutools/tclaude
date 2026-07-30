package db

import (
	"database/sql"
	"fmt"
)

// migrateV176toV177 adds the spawn-profile operator-only gate. Existing
// profiles remain available to agent callers unless an operator opts in.
func migrateV176toV177(d *sql.DB) error {
	if schemaVersion(d) >= 177 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v176→v177 (operator-only spawn profiles): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := addColumnIfMissing(tx, "spawn_profiles", "operator_only",
		`ALTER TABLE spawn_profiles ADD COLUMN operator_only INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("migrate v176→v177 (operator-only spawn profiles): add column: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 177`); err != nil {
		return fmt.Errorf("migrate v176→v177 (operator-only spawn profiles): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v176→v177 (operator-only spawn profiles): commit: %w", err)
	}
	return nil
}
