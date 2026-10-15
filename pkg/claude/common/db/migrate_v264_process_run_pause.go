package db

import (
	"database/sql"
	"fmt"
)

// migrateV263toV264 adds an operator pause to process runs. paused_at is NULL
// for a run that is free to plan; a timestamp parks it — no new agent turns or
// program commands are dispatched — while in-flight attempts finish and are
// recorded. paused_by is the actor ref that paused it. A pause is a hold on
// the run, not an engine transition, so it lives beside the checkpoint rather
// than in it, like scope_state.
func migrateV263toV264(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v263→v264 (process run pause): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	columns := []struct{ name, ddl string }{
		{"paused_at", `ALTER TABLE process_runs ADD COLUMN paused_at INTEGER`},
		{"paused_by", `ALTER TABLE process_runs ADD COLUMN paused_by TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(paused_by AS BLOB)) <= 256)`},
	}
	for _, column := range columns {
		var present int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('process_runs') WHERE name = ?`, column.name).Scan(&present); err != nil {
			return fmt.Errorf("migrate v263→v264 (inspect process_runs): %w", err)
		}
		if present == 0 {
			if _, err := tx.Exec(column.ddl); err != nil {
				return fmt.Errorf("migrate v263→v264 (add %s): %w", column.name, err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 264`); err != nil {
		return fmt.Errorf("migrate v263→v264 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v263→v264 (commit): %w", err)
	}
	return nil
}
