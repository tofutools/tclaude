package db

import "database/sql"

// migrateV287toV288 adds collab_board_history: each actor's undo and redo
// steps on a board. A step is a JSON array of {id, before}, before being the
// element as it stood, or null when the step created it.
func migrateV287toV288(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS collab_board_history (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 board_id TEXT NOT NULL REFERENCES collab_boards(id) ON DELETE CASCADE,
 actor TEXT NOT NULL,
 stack TEXT NOT NULL CHECK (stack IN ('undo', 'redo')),
 body TEXT NOT NULL
) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_collab_board_history_stack ON collab_board_history(board_id, actor, stack, id)`,
		`UPDATE schema_version SET version = 288`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
