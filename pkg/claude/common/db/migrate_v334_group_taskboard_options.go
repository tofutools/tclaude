package db

import "database/sql"

// migrateV333toV334 adds a task board's own options (docs/taskboard.md
// "Board options"): one JSON document per group, holding only what the board
// sets; everything else is inherited from the global config or the built-in
// default (pkg/claude/common/boardopts). version counts every write, so a
// writer that read version n can refuse to overwrite a newer one.
func migrateV333toV334(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_taskboard_options (
			group_id   INTEGER PRIMARY KEY REFERENCES agent_groups(id) ON DELETE CASCADE,
			options    TEXT NOT NULL DEFAULT '{}',
			version    INTEGER NOT NULL DEFAULT 0,
			updated_by TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL
		) STRICT`,
		`UPDATE schema_version SET version = 334`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
