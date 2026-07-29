package db

import (
	"database/sql"
	"fmt"
)

// migrateV173toV174 adds opt-in trailing-edge debounce. The pending table is
// keyed by the durable order+recipient identity; conversation ids are routing
// snapshots only and are re-resolved before delivery.
func migrateV173toV174(d *sql.DB) error {
	if schemaVersion(d) >= 174 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v173→v174 (standing-order debounce): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		ALTER TABLE agent_standing_orders
			ADD COLUMN debounce_seconds INTEGER NOT NULL DEFAULT 0;
		CREATE TABLE agent_standing_order_debounce (
			order_id       INTEGER NOT NULL
			               REFERENCES agent_standing_orders(id) ON DELETE CASCADE,
			order_revision INTEGER NOT NULL,
			target_agent   TEXT NOT NULL,
			target_conv    TEXT NOT NULL,
			epoch          TEXT NOT NULL DEFAULT '',
			harness        TEXT NOT NULL,
			detail         TEXT NOT NULL DEFAULT '',
			due_at         TEXT NOT NULL,
			max_due_at     TEXT NOT NULL,
			updated_at     TEXT NOT NULL,
			PRIMARY KEY (order_id, target_agent)
		);
		CREATE INDEX idx_agent_standing_order_debounce_due
			ON agent_standing_order_debounce(due_at);
		UPDATE schema_version SET version = 174;
	`); err != nil {
		return fmt.Errorf("migrate v173→v174 (standing-order debounce): apply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v173→v174 (standing-order debounce): commit: %w", err)
	}
	return nil
}
