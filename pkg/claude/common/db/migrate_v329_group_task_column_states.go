package db

import "database/sql"

// migrateV328toV329 adds a board-level state to a task board's columns
// (tk_bf5d2b02): a column is shown (no row), hidden (kept, its tasks keep it,
// not drawn) or cordoned (drawn, takes no new task; its tasks may leave).
//
//   - group_task_column_states holds a state other than shown, by the
//     column's slug: a custom column's own, or a built-in column's (ideas,
//     done). Removing a custom column removes its state.
//   - A task moved to another group stays in a column of the same slug there
//     (group_tasks_column_clear) unless that column is cordoned: a cordoned
//     column takes no new task, from another group either.
func migrateV328toV329(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_task_column_states (
			group_id INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			slug     TEXT NOT NULL,
			state    TEXT NOT NULL CHECK (state IN ('hidden', 'cordoned')),
			PRIMARY KEY (group_id, slug)
		) STRICT`,
		`CREATE TRIGGER IF NOT EXISTS group_task_columns_removed_state
			AFTER DELETE ON group_task_columns
			BEGIN DELETE FROM group_task_column_states WHERE group_id = OLD.group_id AND slug = OLD.slug; END`,
		`CREATE TRIGGER IF NOT EXISTS group_tasks_column_cordoned_on_move
			AFTER UPDATE OF group_id ON group_tasks
			WHEN NEW.column_slug <> '' AND OLD.group_id <> NEW.group_id AND EXISTS (SELECT 1 FROM group_task_column_states s
				WHERE s.group_id = NEW.group_id AND s.slug = NEW.column_slug AND s.state = 'cordoned')
			BEGIN UPDATE group_tasks SET column_slug = '' WHERE id = NEW.id; END`,
		`UPDATE schema_version SET version = 329`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
