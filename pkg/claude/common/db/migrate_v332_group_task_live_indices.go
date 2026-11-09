package db

import "database/sql"

// migrateV331toV332 keeps the task board's live reads flat as archived tasks
// accumulate (tk_155354e3). Measured on a board grown to 200k closed tasks,
// every live path walked the archived rows too:
//
//   - idx_group_tasks_status serves the fleet counts by status (blocked,
//     operator-held), which scanned the whole table.
//   - idx_group_tasks_history holds only the closed, not yet archived tasks:
//     the board's recently-closed history and the hourly sweep's archive
//     candidates read it instead of every closed row of the group or table.
//
// Index-only; no rows change, so it is cheap on a large database.
func migrateV331toV332(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_group_tasks_status ON group_tasks(status, group_id)`,
		`CREATE INDEX IF NOT EXISTS idx_group_tasks_history ON group_tasks(group_id, closed_at)
			WHERE archived_at IS NULL AND status IN ('done','dropped')`,
		`UPDATE schema_version SET version = 332`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
