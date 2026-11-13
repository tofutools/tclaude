package db

import "database/sql"

// migrateV335toV336 lets a group template carry a board setup
// (groupexport.Taskboard: board options and custom columns, never tasks),
// which a group instantiated from it is born with. "" is none.
func migrateV335toV336(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// A half-applied old database may not have the table yet; the schema
	// creates it with the column.
	haveTemplates, err := txTableExists(tx, "group_templates")
	if err != nil {
		return err
	}
	if haveTemplates {
		if err := addColumnIfMissing(tx, "group_templates", "taskboard",
			`ALTER TABLE group_templates ADD COLUMN taskboard TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 336`); err != nil {
		return err
	}
	return tx.Commit()
}
