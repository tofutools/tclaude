package db

import (
	"database/sql"
	"fmt"
)

// migrateV170toV171 adds optional normalized-field regex matching to standing
// orders. Both columns default empty, so every existing order keeps matching
// exactly the same events it did before the migration.
func migrateV170toV171(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v170→v171 (standing-order matchers): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, column := range []string{"match_field", "match_regex"} {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agent_standing_orders')
			 WHERE name = ?`, column,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v170→v171 (standing-order matchers): probe %s: %w",
				column, err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE agent_standing_orders ADD COLUMN ` + column +
					` TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v170→v171 (standing-order matchers): add %s: %w",
					column, err)
			}
		}
	}

	if _, err := tx.Exec(`
		CREATE INDEX IF NOT EXISTS idx_agent_standing_orders_enabled_trigger
		ON agent_standing_orders(enabled, trigger_event);
		UPDATE schema_version SET version = 171;
	`); err != nil {
		return fmt.Errorf("migrate v170→v171 (standing-order matchers): finalize: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v170→v171 (standing-order matchers): commit: %w", err)
	}
	return nil
}
