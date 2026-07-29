package db

import (
	"database/sql"
	"fmt"
)

// migrateV175toV176 adds harness-native OR branches for standing-order
// triggers. The table starts empty, so every existing order keeps using its
// normalized trigger_event exactly as before. Native hook declarations are
// installed only after an operator explicitly authors and enables selectors.
func migrateV175toV176(d *sql.DB) error {
	if schemaVersion(d) >= 176 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v175→v176 (standing-order hook selectors): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE agent_standing_order_hook_selectors (
			order_id INTEGER NOT NULL
			         REFERENCES agent_standing_orders(id) ON DELETE CASCADE,
			harness  TEXT NOT NULL,
			event    TEXT NOT NULL,
			PRIMARY KEY (order_id, harness, event)
		);
		CREATE INDEX idx_agent_standing_order_hook_selectors_event
			ON agent_standing_order_hook_selectors(harness, event, order_id);
		UPDATE schema_version SET version = 176;
	`); err != nil {
		return fmt.Errorf("migrate v175→v176 (standing-order hook selectors): apply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v175→v176 (standing-order hook selectors): commit: %w", err)
	}
	return nil
}
