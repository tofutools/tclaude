package db

import "database/sql"

// migrateV309toV310 adds group_pages (tcl-1lth8e): the live pages shared in
// an agent group, saved so a closed page reopens from the group and a page
// survives a daemon restart. A page's stage token is never stored: reopening
// mints a fresh origin. Pages shown privately (no group) are not saved.
func migrateV309toV310(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_pages (
			id            TEXT PRIMARY KEY,
			group_id      INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			owner         TEXT NOT NULL,
			name          TEXT NOT NULL,
			title         TEXT NOT NULL DEFAULT '',
			kind          TEXT NOT NULL,
			target        TEXT NOT NULL DEFAULT '',
			path          TEXT NOT NULL DEFAULT '',
			worktree      TEXT NOT NULL DEFAULT '',
			port          INTEGER NOT NULL DEFAULT 0,
			agents_locked INTEGER NOT NULL DEFAULT 0,
			state         TEXT NOT NULL CHECK(state IN ('open','closed')),
			closed_reason TEXT NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			closed_at     INTEGER,
			UNIQUE (group_id, owner, name)
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_group_pages_group ON group_pages(group_id, state)`,
		`UPDATE schema_version SET version = 310`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
