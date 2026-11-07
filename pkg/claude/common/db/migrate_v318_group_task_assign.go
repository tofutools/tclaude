package db

import "database/sql"

// migrateV317toV318 adds what the task board's assign/report slice
// (tcl-2eeakl) stores beyond the V1.1 columns:
//
//   - group_taskboard_locks: a group's writer lock. While a row exists, a
//     member's membership-conferred write (and take/release) is narrowed to the
//     listed writers; owners, explicit grant holders and the operator are not
//     affected. Writers are agent ids, kept as a JSON array.
//   - a trigger that unassigns an agent's live tasks when the agent is retired,
//     whichever path retires it, and records it in the task's history.
func migrateV317toV318(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_taskboard_locks (
			group_id   INTEGER PRIMARY KEY REFERENCES agent_groups(id) ON DELETE CASCADE,
			writers    TEXT NOT NULL DEFAULT '[]',
			set_by     TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_group_tasks_assignee ON group_tasks(assignee_id) WHERE assignee_id IS NOT NULL`,
		`CREATE TRIGGER IF NOT EXISTS group_tasks_unassign_on_retire
		AFTER UPDATE OF retired_at ON agents
		WHEN NEW.retired_at IS NOT NULL AND OLD.retired_at IS NULL
		BEGIN
			INSERT INTO group_task_events (task_id, at, actor, kind, detail)
				SELECT id, NEW.retired_at, 'system:retire', 'unassigned', 'assignee retired'
				FROM group_tasks WHERE assignee_id = NEW.agent_id AND status IN ('open','blocked');
			UPDATE group_tasks SET assignee_id = NULL, assigned_by = '', assigned_at = NULL, updated_at = NEW.retired_at
				WHERE assignee_id = NEW.agent_id AND status IN ('open','blocked');
		END`,
		`UPDATE schema_version SET version = 318`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
