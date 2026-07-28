package db

import (
	"database/sql"
	"fmt"
)

// migrateV167toV168 adds the optional persistent reference attached to a
// group (TCL-801). The URL and its optional display-label override live on
// agent_groups so they survive daemon restarts and group renames.
//
// Both ADD COLUMN operations are probe-guarded so a partially applied
// migration converges cleanly on restart.
func migrateV167toV168(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v167→v168 (group attachment): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_groups'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v167→v168 (group attachment): probe table: %w", err)
	}
	// Missing-table tolerance is for the repository's minimal historical
	// migration-heal fixtures, matching the v166/v167 convention. Production
	// creates agent_groups only in v8, before this step; there is no later
	// creator that could leave a v168 database with this table but without
	// the attachment columns.
	if haveTable > 0 {
		for _, col := range []string{"attachment_url", "attachment_label"} {
			var have int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = ?`, col,
			).Scan(&have); err != nil {
				return fmt.Errorf("migrate v167→v168 (group attachment): probe %s: %w", col, err)
			}
			if have == 0 {
				if _, err := tx.Exec(`ALTER TABLE agent_groups ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
					return fmt.Errorf("migrate v167→v168 (group attachment): add %s: %w", col, err)
				}
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 168`); err != nil {
		return fmt.Errorf("migrate v167→v168 (group attachment): set version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v167→v168 (group attachment): commit: %w", err)
	}
	return nil
}
