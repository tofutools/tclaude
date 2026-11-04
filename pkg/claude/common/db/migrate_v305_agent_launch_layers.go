package db

import (
	"database/sql"
	"fmt"
)

// migrateV304toV305 adds `agents.launch_layers`: the per-agent launch layers
// a relaunch resolves from (see AgentLaunchLayers), kept apart from
// relaunch_profile because session projection rewrites that one on every
// session save, and the spawn-profile copy must be taken once.
//
// Additive and idempotent, like v84: the table and column are probed, and
// NOT NULL DEFAULT '' marks every existing agent as pre-change ("no layers
// recorded"), which a relaunch treats as seeded from its stored launch shape.
// No data pass and nothing rewritten, so no backup is taken.
func migrateV304toV305(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v304→v305: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agents")
	if err != nil {
		return fmt.Errorf("migrate v304→v305 (probe agents): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = 'launch_layers'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v304→v305 (probe agents.launch_layers): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE agents ADD COLUMN launch_layers TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v304→v305 (add agents.launch_layers): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 305`); err != nil {
		return fmt.Errorf("migrate v304→v305 (version): %w", err)
	}
	return tx.Commit()
}
