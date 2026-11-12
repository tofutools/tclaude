package db

import "database/sql"

// migrateV334toV335 adds a task's place in a work-in-progress queue
// (docs/taskboard.md "Limits"): queued_for names the full lane the task waits
// for ('in_progress' or a custom column's slug), queued_at when it got in
// line. A task leaves the queue when it gets into its lane, and whenever
// what it queued as changes: it closes, moves to another group, changes
// hands, is demoted to the ideas, or moves to another column.
func migrateV334toV335(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_tasks ADD COLUMN queued_for TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE group_tasks ADD COLUMN queued_at INTEGER`,
		`CREATE INDEX IF NOT EXISTS idx_group_tasks_queued ON group_tasks(group_id, queued_at) WHERE queued_for <> ''`,
		// queued_for itself is not in the column list: the write that puts
		// a task in line does not fire it.
		`CREATE TRIGGER IF NOT EXISTS group_tasks_queued_clear
			AFTER UPDATE OF status, group_id, assignee_id, assignee_kind, started_at, column_slug, stage ON group_tasks
			WHEN NEW.queued_for <> '' AND (
				NEW.status NOT IN ('open','blocked')
				OR NEW.group_id <> OLD.group_id
				OR NEW.assignee_id IS NOT OLD.assignee_id
				OR NEW.assignee_kind <> OLD.assignee_kind
				OR NEW.column_slug <> OLD.column_slug
				OR NEW.stage = 'idea'
				OR (NEW.started_at IS NOT NULL) <> (OLD.started_at IS NOT NULL))
			BEGIN UPDATE group_tasks SET queued_for = '', queued_at = NULL WHERE id = NEW.id; END`,
		`CREATE TRIGGER IF NOT EXISTS group_task_columns_removed_queue
			AFTER DELETE ON group_task_columns
			BEGIN UPDATE group_tasks SET queued_for = '', queued_at = NULL WHERE group_id = OLD.group_id AND queued_for = OLD.slug; END`,
		`UPDATE schema_version SET version = 335`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
