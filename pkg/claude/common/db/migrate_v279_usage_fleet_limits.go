package db

import (
	"database/sql"
	"fmt"
)

// migrateV278toV279 adds usage_fleet_limits: one rule over spend summed
// across trusted peers. target names the nodes: "all" (every trusted peer
// sharing usage.read), "pool:<pool id>" (a local node pool, a sub-fleet) or
// "peer:<instance id>"; include_self adds this node's own spend.
func migrateV278toV279(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v278→v279 (usage fleet limits): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS usage_fleet_limits (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    target       TEXT NOT NULL,
    include_self INTEGER NOT NULL DEFAULT 1,
    rule         TEXT NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
) STRICT`); err != nil {
		return fmt.Errorf("migrate v278→v279 (usage fleet limits): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 279`); err != nil {
		return fmt.Errorf("migrate v278→v279 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v278→v279 (commit): %w", err)
	}
	return nil
}
