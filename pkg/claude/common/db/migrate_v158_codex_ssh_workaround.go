package db

import (
	"database/sql"
	"fmt"
)

// migrateV157toV158 adds the tri-state Codex SSH compatibility toggle to spawn
// profiles. NULL inherits the Codex default (on); 0 opts out; 1 opts in.
func migrateV157toV158(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v157→v158 (Codex SSH workaround): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var have int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles')
		WHERE name = 'ssh_workaround'`).Scan(&have); err != nil {
		return fmt.Errorf("migrate v157→v158 (Codex SSH workaround): probe: %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN ssh_workaround INTEGER`); err != nil {
			return fmt.Errorf("migrate v157→v158 (Codex SSH workaround): add column: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 158`); err != nil {
		return fmt.Errorf("migrate v157→v158 (Codex SSH workaround): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v157→v158 (Codex SSH workaround): commit: %w", err)
	}
	return nil
}
