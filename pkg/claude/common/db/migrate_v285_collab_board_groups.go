package db

import "database/sql"

// A collab board can belong to a group (tcl-4yhow7): the group's active
// members are on the board without being invited one by one. A deleted group
// leaves the board unlinked.
func migrateV284toV285(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE collab_boards ADD COLUMN group_id INTEGER REFERENCES agent_groups(id) ON DELETE SET NULL`,
		`CREATE INDEX IF NOT EXISTS idx_collab_boards_group ON collab_boards(group_id)`,
		`UPDATE schema_version SET version = 285`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
