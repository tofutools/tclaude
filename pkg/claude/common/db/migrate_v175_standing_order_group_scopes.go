package db

import (
	"database/sql"
	"fmt"
)

// migrateV174toV175 lets one reusable standing-order definition be activated
// on additional groups. Existing rows keep their exact primary target and
// therefore preserve behavior; the new table is empty until an operator opts
// a group in from the dashboard.
func migrateV174toV175(d *sql.DB) error {
	if schemaVersion(d) >= 175 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v174→v175 (standing-order group scopes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE agent_standing_order_group_scopes (
			order_id  INTEGER NOT NULL
			          REFERENCES agent_standing_orders(id) ON DELETE CASCADE,
			group_id  INTEGER NOT NULL
			          REFERENCES agent_groups(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL,
			PRIMARY KEY (order_id, group_id)
		);
		CREATE INDEX idx_agent_standing_order_group_scopes_group
			ON agent_standing_order_group_scopes(group_id, order_id);
		UPDATE schema_version SET version = 175;
	`); err != nil {
		return fmt.Errorf("migrate v174→v175 (standing-order group scopes): apply: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v174→v175 (standing-order group scopes): commit: %w", err)
	}
	return nil
}
