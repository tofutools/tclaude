package db

import "database/sql"

// migrateV329toV330 lets a task have several refs (tk_01ad0884): ref_kind,
// ref_url and ref_label stay the task's first ref, so every reader of the
// one ref keeps working, and more_refs holds the rest, in order, as a JSON
// array of {"kind","url","label"} (empty for none). Existing tasks have none
// more: nothing to move.
func migrateV329toV330(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_tasks ADD COLUMN more_refs TEXT NOT NULL DEFAULT ''`,
		`UPDATE schema_version SET version = 330`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
