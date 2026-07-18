package db

import (
	"database/sql"
	"fmt"
)

// migrateV134toV135 adds the local operator's reusable process-editor
// snippets. The graph payload is the versioned process-selection envelope
// owned by the editor, not another process graph representation. A singleton
// library row serializes quota checks with writes and carries a collection
// generation for observability; item revisions provide mutation CAS.
//
// The migration is additive, so an older binary can continue to use the same
// database without reading or mutating this table. Both CREATEs and the seed
// INSERT are idempotent so a restart after a partially applied migration
// converges safely.
func migrateV134toV135(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v134→v135: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS process_snippet_library (
			id         INTEGER PRIMARY KEY CHECK(id = 1),
			generation INTEGER NOT NULL CHECK(generation >= 0)
		);
		INSERT OR IGNORE INTO process_snippet_library(id, generation) VALUES (1, 0);

		CREATE TABLE IF NOT EXISTS process_snippets (
			id            TEXT PRIMARY KEY,
			name          TEXT NOT NULL,
			name_key      TEXT NOT NULL UNIQUE,
			envelope_json TEXT NOT NULL,
			revision      INTEGER NOT NULL CHECK(revision > 0),
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_process_snippets_order
			ON process_snippets(name_key, id);
	`); err != nil {
		return fmt.Errorf("migrate v134→v135 (create process snippets): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 135`); err != nil {
		return fmt.Errorf("migrate v134→v135 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v134→v135 (commit): %w", err)
	}
	return nil
}
