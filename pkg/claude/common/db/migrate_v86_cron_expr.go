package db

import (
	"database/sql"
	"fmt"
)

// migrateV85toV86 adds `agent_cron_jobs.cron_expr` — an optional standard
// cron expression (5-field or @descriptor, robfig/cron/v3 syntax) as an
// alternative schedule to the fixed interval. Empty string = interval mode
// (the long-standing shape); non-empty = expression mode, in which
// interval_seconds is 0 and the scheduler's due check evaluates the
// expression's next fire time after last_run_at (or created_at for a job
// that has never run — an expression job waits for its first match rather
// than firing immediately on creation).
//
// Additive + idempotent (the v76–v85 convention): the table is probed so a
// partial-schema heal DB without `agent_cron_jobs` is a clean skip; the ADD
// COLUMN is guarded by a pragma_table_info probe so a half-applied run
// converges on re-run instead of wedging on "duplicate column". TEXT NOT
// NULL DEFAULT '' back-fills every existing row to interval mode with no
// data pass. One transaction with the version bump; nothing is dropped or
// rewritten.
func migrateV85toV86(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v85→v86: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agent_cron_jobs")
	if err != nil {
		return fmt.Errorf("migrate v85→v86 (probe agent_cron_jobs): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_cron_jobs') WHERE name = 'cron_expr'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v85→v86 (probe agent_cron_jobs.cron_expr): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE agent_cron_jobs ADD COLUMN cron_expr TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v85→v86 (add agent_cron_jobs.cron_expr): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 86`); err != nil {
		return fmt.Errorf("migrate v85→v86 (version): %w", err)
	}
	return tx.Commit()
}
