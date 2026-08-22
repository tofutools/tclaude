package db

import (
	"database/sql"
	"fmt"
)

// migrateV220toV221 adds a per-session launch evidence record. Keeping it in
// a companion table avoids widening the hot sessions UPSERT and lets legacy
// launches remain explicitly unrecorded.
func migrateV220toV221(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v220→v221 (session execution boundaries): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS session_execution_boundaries (
		session_id    TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
		boundary_json TEXT NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("migrate v220→v221 (session execution boundaries): create table: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 221`); err != nil {
		return fmt.Errorf("migrate v220→v221 (session execution boundaries): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v220→v221 (session execution boundaries): commit: %w", err)
	}
	return nil
}
