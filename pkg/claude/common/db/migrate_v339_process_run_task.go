package db

import (
	"database/sql"
	"fmt"
)

// migrateV338toV339 lets a process run be for a board task (tk_8aa88514): the
// task's id, "" for a run that is for none. It also adds a task's own choice
// of process (group_task_process). The task card reads its run by it,
// and the run's end settles the task. The index serves both directions: the
// runs of a task, and whether a task already has a live one.
func migrateV338toV339(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v338→v339 (process run task): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('process_runs') WHERE name = 'task_id'`).Scan(&present); err != nil {
		return fmt.Errorf("migrate v338→v339 (inspect process_runs): %w", err)
	}
	if present == 0 {
		if _, err := tx.Exec(`ALTER TABLE process_runs ADD COLUMN task_id TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(task_id AS BLOB)) <= 128)`); err != nil {
			return fmt.Errorf("migrate v338→v339 (add task_id): %w", err)
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS process_runs_task ON process_runs(task_id) WHERE task_id <> ''`); err != nil {
		return fmt.Errorf("migrate v338→v339 (index task_id): %w", err)
	}
	// A task's own choice of process, against its board's default way of
	// working: a template id, or "none". A task with no row follows the board.
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS group_task_process (
		task_id TEXT PRIMARY KEY REFERENCES group_tasks(id) ON DELETE CASCADE,
		process TEXT NOT NULL CHECK(length(CAST(process AS BLOB)) BETWEEN 1 AND 128)
	)`); err != nil {
		return fmt.Errorf("migrate v338→v339 (group_task_process): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 339`); err != nil {
		return fmt.Errorf("migrate v338→v339 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v338→v339 (commit): %w", err)
	}
	return nil
}
