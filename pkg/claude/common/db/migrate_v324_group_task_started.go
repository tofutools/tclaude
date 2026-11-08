package db

import "database/sql"

// migrateV323toV324 lets an assignee hold a task without starting it
// (tk_3826822a): started_at is when the assignee started it. A live task
// with an assignee and no started_at sits in its stage (an idea to triage,
// or queued in next) in the assignee's lane; with one it is in progress.
// Every task assigned today counts as started, so nothing moves.
func migrateV323toV324(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_tasks ADD COLUMN started_at INTEGER`,
		`UPDATE group_tasks SET started_at = COALESCE(assigned_at, updated_at)
			WHERE (assignee_id IS NOT NULL OR assignee_kind = 'operator')`,
		`UPDATE schema_version SET version = 324`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
