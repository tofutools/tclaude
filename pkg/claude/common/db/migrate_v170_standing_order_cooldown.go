package db

import (
	"database/sql"
	"fmt"
)

// migrateV169toV170 adds the first advanced standing-order rate control.
//
// cooldown_seconds is inert by default. target_agent is copied onto new
// ledger rows so a cooldown follows the durable recipient across /clear,
// reincarnation, and any other conversation-generation rotation.
func migrateV169toV170(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v169→v170 (standing-order cooldown): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveCooldown int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_standing_orders')
		 WHERE name = 'cooldown_seconds'`,
	).Scan(&haveCooldown); err != nil {
		return fmt.Errorf("migrate v169→v170 (standing-order cooldown): probe order column: %w", err)
	}
	if haveCooldown == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE agent_standing_orders
			 ADD COLUMN cooldown_seconds INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migrate v169→v170 (standing-order cooldown): add order column: %w", err)
		}
	}

	var haveTargetAgent int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_standing_order_deliveries')
		 WHERE name = 'target_agent'`,
	).Scan(&haveTargetAgent); err != nil {
		return fmt.Errorf("migrate v169→v170 (standing-order cooldown): probe ledger column: %w", err)
	}
	if haveTargetAgent == 0 {
		if _, err := tx.Exec(
			`ALTER TABLE agent_standing_order_deliveries
			 ADD COLUMN target_agent TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migrate v169→v170 (standing-order cooldown): add ledger column: %w", err)
		}
	}

	if _, err := tx.Exec(`
		CREATE INDEX IF NOT EXISTS idx_agent_standing_order_deliveries_cooldown
		ON agent_standing_order_deliveries(
			order_id, order_revision, target_agent, id
		);
		UPDATE schema_version SET version = 170;
	`); err != nil {
		return fmt.Errorf("migrate v169→v170 (standing-order cooldown): finalize: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v169→v170 (standing-order cooldown): commit: %w", err)
	}
	return nil
}
