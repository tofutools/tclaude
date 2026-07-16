package db

import (
	"database/sql"
	"fmt"
)

// migrateV124toV125 adds the operator-managed cross-harness spawn matrix.
// group_id=0 is the global scope; positive IDs are group overrides. A missing
// edge means allow globally and inherit at group scope, preserving all
// pre-migration spawn behavior.
func migrateV124toV125(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v124→v125 (spawn harness rules): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS spawn_harness_rules (
			group_id       INTEGER NOT NULL DEFAULT 0,
			source_harness TEXT NOT NULL,
			target_harness TEXT NOT NULL,
			decision       TEXT NOT NULL CHECK (decision IN ('allow', 'deny')),
			reason         TEXT NOT NULL DEFAULT '',
			updated_at     TEXT NOT NULL,
			PRIMARY KEY (group_id, source_harness, target_harness),
			CHECK (source_harness <> target_harness)
		);
		CREATE INDEX IF NOT EXISTS idx_spawn_harness_rules_group
			ON spawn_harness_rules(group_id);
	`); err != nil {
		return fmt.Errorf("migrate v124→v125 (spawn harness rules): create table: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 125`); err != nil {
		return fmt.Errorf("migrate v124→v125 (spawn harness rules): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v124→v125 (spawn harness rules): commit: %w", err)
	}
	return nil
}
