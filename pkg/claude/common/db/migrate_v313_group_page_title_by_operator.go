package db

import "database/sql"

// migrateV312toV313 records whether the operator named a saved page
// (tcl-kostyh): an agent's show without a title keeps the operator's.
func migrateV312toV313(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_pages ADD COLUMN title_by_operator INTEGER NOT NULL DEFAULT 0`,
		`UPDATE schema_version SET version = 313`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
