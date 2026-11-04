package db

import "database/sql"

// migrateV310toV311 saves a group page's marks and live patches with it
// (tcl-01arix): one row per mark batch the operator sent, and one row per
// page holding its patch journal. Both go with the page's group_pages row.
func migrateV310toV311(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_page_marks (
			page_id TEXT NOT NULL REFERENCES group_pages(id) ON DELETE CASCADE,
			seq     INTEGER NOT NULL,
			batch   TEXT NOT NULL,
			PRIMARY KEY (page_id, seq)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS group_page_patches (
			page_id   TEXT PRIMARY KEY REFERENCES group_pages(id) ON DELETE CASCADE,
			mark_seq  INTEGER NOT NULL DEFAULT 0,
			patch_seq INTEGER NOT NULL DEFAULT 0,
			patches   TEXT NOT NULL DEFAULT '[]'
		) STRICT`,
		`UPDATE schema_version SET version = 311`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
