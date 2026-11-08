package db

import "database/sql"

// migrateV329toV330 lets a task's file say it is a drawing-board export
// (tk_413bdf5d): drawing is the export's JSON reference back to the board
// (which board, at which version, which part of it), so the viewer can offer
// "Edit in drawing board" and a re-export can update the file in place.
// NULL for every other file.
func migrateV329toV330(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_task_attachments ADD COLUMN drawing TEXT`,
		`UPDATE schema_version SET version = 330`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
