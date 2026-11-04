package db

import (
	"database/sql"
	"fmt"
)

// migrateV307toV308 adds `relaunch_policy`: the stored answers of the
// relaunch policy (launchspec.RelaunchPolicy), one row per scope and
// configurable launch field. A field with no row takes its default mode, so
// an empty table is the default policy and nothing is seeded.
//
// Additive and idempotent: CREATE TABLE IF NOT EXISTS, no backup.
func migrateV307toV308(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v307→v308: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS relaunch_policy (
			scope      TEXT NOT NULL CHECK(length(scope) BETWEEN 1 AND 64),
			field      TEXT NOT NULL CHECK(length(field) BETWEEN 1 AND 64),
			mode       TEXT NOT NULL CHECK(length(mode) BETWEEN 1 AND 64),
			updated_at INTEGER NOT NULL,
			updated_by TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (scope, field)
		) STRICT`); err != nil {
		return fmt.Errorf("migrate v307→v308 (create relaunch_policy): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 308`); err != nil {
		return fmt.Errorf("migrate v307→v308 (version): %w", err)
	}
	return tx.Commit()
}
