package db

import "database/sql"

// migrateV320toV321 adds files attached to a task on a group's task board
// (tcl-jy91ej). A row names a content-addressed blob the daemon keeps under
// its data directory by its sha256, so a clone or a move shares the bytes
// and a blob no row names any more is collected. Removing a task (the
// retention purge, a deleted group) removes its rows with it.
func migrateV320toV321(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_task_attachments (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id      TEXT NOT NULL REFERENCES group_tasks(id) ON DELETE CASCADE,
			name         TEXT NOT NULL,
			sha256       TEXT NOT NULL,
			size_bytes   INTEGER NOT NULL,
			content_type TEXT NOT NULL,
			added_by     TEXT NOT NULL,
			added_at     INTEGER NOT NULL
		) STRICT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_group_task_attachments_name ON group_task_attachments(task_id, name COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_group_task_attachments_sha ON group_task_attachments(sha256)`,
		`UPDATE schema_version SET version = 321`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
