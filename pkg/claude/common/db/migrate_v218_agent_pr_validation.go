package db

import (
	"database/sql"
	"fmt"
)

// migrateV217toV218 quarantines presented PR rows created before repository
// validation existed. Only a fresh presentation may populate the proof root.
func migrateV217toV218(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v217→v218 (presented PR validation): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := addColumnIfMissing(tx, "agent_prs", "validated_repo_root",
		`ALTER TABLE agent_prs ADD COLUMN validated_repo_root TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrate v217→v218 (presented PR validation): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 218`); err != nil {
		return fmt.Errorf("migrate v217→v218 (presented PR validation): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v217→v218 (presented PR validation): commit: %w", err)
	}
	return nil
}
