package db

import "database/sql"

// migrateV324toV325 adds custom stage columns to a group's task board
// (tcl-plby5h): a group may name a few columns of its own ("In review",
// "In CI") that sit between In progress and Blocked.
//
//   - group_task_columns holds a group's custom columns in their order. The
//     slug is fixed at creation; the label can be renamed.
//   - group_tasks.column_slug names the custom column a live task of stage
//     next stands in, '' for none. The triggers keep it consistent: it is
//     cleared when the task leaves next, is closed, or moves to a group with
//     no column of that slug, and when its column is removed.
func migrateV324toV325(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_task_columns (
			group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			slug       TEXT NOT NULL,
			label      TEXT NOT NULL,
			position   INTEGER NOT NULL,
			created_by TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			PRIMARY KEY (group_id, slug)
		) STRICT`,
		`ALTER TABLE group_tasks ADD COLUMN column_slug TEXT NOT NULL DEFAULT ''`,
		`CREATE TRIGGER IF NOT EXISTS group_tasks_column_clear
			AFTER UPDATE OF stage, status, group_id ON group_tasks
			WHEN NEW.column_slug <> '' AND (NEW.stage <> 'next' OR NEW.status NOT IN ('open','blocked')
				OR NOT EXISTS (SELECT 1 FROM group_task_columns c WHERE c.group_id = NEW.group_id AND c.slug = NEW.column_slug))
			BEGIN UPDATE group_tasks SET column_slug = '' WHERE id = NEW.id; END`,
		`CREATE TRIGGER IF NOT EXISTS group_task_columns_removed
			AFTER DELETE ON group_task_columns
			BEGIN UPDATE group_tasks SET column_slug = '' WHERE group_id = OLD.group_id AND column_slug = OLD.slug; END`,
		`UPDATE schema_version SET version = 325`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
