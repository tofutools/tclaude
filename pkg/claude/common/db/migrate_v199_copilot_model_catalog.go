package db

import (
	"database/sql"
	"fmt"
)

// migrateV198toV199 adds the daemon's mirror of Copilot's authenticated model
// catalog. The prompt limit is the context-meter denominator Copilot exposes
// through both its remote /models response and the local UI-server context RPC.
// The other limits and raw model object are retained so the mirror remains
// useful without another migration when a later consumer needs more than the
// denominator.
func migrateV198toV199(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v198→v199 (Copilot model catalog): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS copilot_model_catalog (
			model_id                  TEXT PRIMARY KEY,
			max_context_window_tokens INTEGER NOT NULL DEFAULT 0 CHECK (max_context_window_tokens >= 0),
			max_prompt_tokens         INTEGER NOT NULL DEFAULT 0 CHECK (max_prompt_tokens >= 0),
			max_output_tokens         INTEGER NOT NULL DEFAULT 0 CHECK (max_output_tokens >= 0),
			fetched_at                INTEGER NOT NULL,
			raw_json                  TEXT NOT NULL DEFAULT ''
		) STRICT;
	`); err != nil {
		return fmt.Errorf("migrate v198→v199 (Copilot model catalog): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 199`); err != nil {
		return fmt.Errorf("migrate v198→v199 (Copilot model catalog): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v198→v199 (Copilot model catalog): commit: %w", err)
	}
	return nil
}
