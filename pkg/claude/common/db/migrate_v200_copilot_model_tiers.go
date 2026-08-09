package db

import (
	"database/sql"
	"fmt"
)

// migrateV199toV200 retains the account-enriched Copilot CLI model record and
// its long-context prompt budget alongside the unfiltered remote catalog row.
func migrateV199toV200(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v199→v200 (Copilot model tiers): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS copilot_model_catalog (
			model_id                          TEXT PRIMARY KEY,
			max_context_window_tokens         INTEGER NOT NULL DEFAULT 0 CHECK (max_context_window_tokens >= 0),
			max_prompt_tokens                 INTEGER NOT NULL DEFAULT 0 CHECK (max_prompt_tokens >= 0),
			long_context_max_prompt_tokens    INTEGER NOT NULL DEFAULT 0 CHECK (long_context_max_prompt_tokens >= 0),
			max_output_tokens                 INTEGER NOT NULL DEFAULT 0 CHECK (max_output_tokens >= 0),
			fetched_at                        INTEGER NOT NULL,
			raw_json                          TEXT NOT NULL DEFAULT '',
			enriched_json                     TEXT NOT NULL DEFAULT ''
		) STRICT;
	`); err != nil {
		return fmt.Errorf("migrate v199→v200 (Copilot model tiers): create: %w", err)
	}

	for _, column := range []struct {
		name string
		sql  string
	}{
		{"long_context_max_prompt_tokens", `INTEGER NOT NULL DEFAULT 0 CHECK (long_context_max_prompt_tokens >= 0)`},
		{"enriched_json", `TEXT NOT NULL DEFAULT ''`},
	} {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('copilot_model_catalog') WHERE name = ?`,
			column.name,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v199→v200 (Copilot model tiers): probe %s: %w", column.name, err)
		}
		if have == 0 {
			if _, err := tx.Exec(`ALTER TABLE copilot_model_catalog ADD COLUMN ` + column.name + ` ` + column.sql); err != nil {
				return fmt.Errorf("migrate v199→v200 (Copilot model tiers): add %s: %w", column.name, err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 200`); err != nil {
		return fmt.Errorf("migrate v199→v200 (Copilot model tiers): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v199→v200 (Copilot model tiers): commit: %w", err)
	}
	return nil
}
