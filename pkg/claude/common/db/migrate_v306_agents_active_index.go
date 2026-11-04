package db

import (
	"database/sql"
	"fmt"
)

// migrateV305toV306 adds a partial index over the active agents
// (retired_at IS NULL), keyed by agent_id. Fleet-wide scans of active agents,
// such as the harness_config repair finder and the spawn-copy lookup behind
// profile re-apply, otherwise read every agents row in primary-key order, and
// a long-lived install can hold thousands of retired agents. With the index
// such a scan visits the active rows only, already in agent_id order.
//
// Additive and idempotent: IF NOT EXISTS, nothing rewritten, no backup.
func migrateV305toV306(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v305→v306: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agents")
	if err != nil {
		return fmt.Errorf("migrate v305→v306 (probe agents): %w", err)
	}
	if haveTable {
		if _, err := tx.Exec(
			`CREATE INDEX IF NOT EXISTS idx_agents_active ON agents(agent_id) WHERE retired_at IS NULL`,
		); err != nil {
			return fmt.Errorf("migrate v305→v306 (index active agents): %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 306`); err != nil {
		return fmt.Errorf("migrate v305→v306 (version): %w", err)
	}
	return tx.Commit()
}
