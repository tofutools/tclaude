package db

import (
	"database/sql"
	"fmt"
)

// migrateV171toV172 separates standing-order write concurrency from delivery
// cadence. revision remains the delivery/content revision recorded in the
// ledger; row_version becomes the monotonic compare-and-swap token.
func migrateV171toV172(d *sql.DB) error {
	if schemaVersion(d) >= 172 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v171→v172 (standing-order row version): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var have int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('agent_standing_orders')
		 WHERE name = 'row_version'`,
	).Scan(&have); err != nil {
		return fmt.Errorf("migrate v171→v172 (standing-order row version): probe: %w", err)
	}
	if have == 0 {
		if _, err := tx.Exec(`
			ALTER TABLE agent_standing_orders
			ADD COLUMN row_version INTEGER NOT NULL DEFAULT 1;
		`); err != nil {
			return fmt.Errorf("migrate v171→v172 (standing-order row version): add column: %w", err)
		}
	}
	// Copying revision preserves distinct tokens for rows edited before the
	// migration. A constant backfill would make previously different editor
	// snapshots indistinguishable at the new CAS boundary.
	if _, err := tx.Exec(`
		UPDATE agent_standing_orders SET row_version = revision;
		UPDATE schema_version SET version = 172;
	`); err != nil {
		return fmt.Errorf("migrate v171→v172 (standing-order row version): finalize: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v171→v172 (standing-order row version): commit: %w", err)
	}
	return nil
}
