package db

import "database/sql"

// migrateV267toV268 adds queued_spawns: spawn-dialog drafts the operator
// prepared but chose not to launch yet. The draft is the dashboard's own form
// state, stored opaquely so reopening the dialog restores it exactly; launching
// one replays it through the normal spawn path.
func migrateV267toV268(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS queued_spawns (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	group_name TEXT NOT NULL,
	draft      TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
) STRICT;
UPDATE schema_version SET version = 268;`); err != nil {
		return err
	}
	return tx.Commit()
}
