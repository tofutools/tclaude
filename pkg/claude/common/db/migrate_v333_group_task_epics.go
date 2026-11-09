package db

import "database/sql"

// migrateV332toV333 adds epics to the task board (tk_a970ef38): a task of
// kind epic is a container, and any other task may name one epic of its
// group as its parent. One level: an epic has no parent and is no child.
//
// close_suggested_at marks an open epic whose children are all closed (the
// board shows a "close epic?" nudge; nothing closes it by itself);
// close_nudged records that its creator was mailed about it, once per
// suggestion. idx_group_tasks_parent serves an epic's children and the
// per-epic progress counts.
func migrateV332toV333(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_tasks ADD COLUMN kind TEXT NOT NULL DEFAULT '' CHECK(kind IN ('','epic'))`,
		`ALTER TABLE group_tasks ADD COLUMN parent_id TEXT REFERENCES group_tasks(id) ON DELETE SET NULL`,
		`ALTER TABLE group_tasks ADD COLUMN close_suggested_at INTEGER`,
		`ALTER TABLE group_tasks ADD COLUMN close_nudged INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_group_tasks_parent ON group_tasks(parent_id) WHERE parent_id IS NOT NULL`,
		`UPDATE schema_version SET version = 333`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
