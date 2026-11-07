package db

import "database/sql"

// migrateV321toV322 turns a task's history into its activity stream
// (tcl-jipq70), modelled on awb's: comments live in group_task_events beside
// the change entries, and a change entry may carry its field changes.
//
//   - body is a comment's Markdown (a comment_edited entry: the body it had
//     before that edit); comment_kind says what the comment is (a plain
//     comment, or a block, done or drop reason or a report note committed
//     with that transition).
//   - client_key makes a comment idempotent per task and author.
//   - comment_id ties a comment_edited entry to its comment; edited_at and
//     deleted_at mark a comment edited or deleted (a tombstone).
//   - changes is a field change's [{field, before, after}] as JSON.
//
// A comment's files are task attachments that name it (comment_id), and go
// with it.
func migrateV321toV322(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_task_events ADD COLUMN body TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE group_task_events ADD COLUMN comment_kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE group_task_events ADD COLUMN client_key TEXT`,
		`ALTER TABLE group_task_events ADD COLUMN comment_id INTEGER`,
		`ALTER TABLE group_task_events ADD COLUMN edited_at INTEGER`,
		`ALTER TABLE group_task_events ADD COLUMN deleted_at INTEGER`,
		`ALTER TABLE group_task_events ADD COLUMN changes TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_group_task_events_key ON group_task_events(task_id, actor, client_key)
			WHERE client_key IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_group_task_events_kind ON group_task_events(task_id, kind)`,
		`ALTER TABLE group_task_attachments ADD COLUMN comment_id INTEGER REFERENCES group_task_events(id) ON DELETE CASCADE`,
		`CREATE INDEX IF NOT EXISTS idx_group_task_attachments_comment ON group_task_attachments(comment_id)
			WHERE comment_id IS NOT NULL`,
		`UPDATE schema_version SET version = 322`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
