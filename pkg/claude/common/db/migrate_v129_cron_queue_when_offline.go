package db

import (
	"database/sql"
	"fmt"
)

// migrateV128toV129 adds the cron job's explicit opt-in for durable delivery
// while a recipient is offline. Existing jobs default to the safer behaviour:
// an offline tick is recorded and discarded instead of becoming inbox debt.
func migrateV128toV129(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	haveCron, err := txTableExists(tx, "agent_cron_jobs")
	if err != nil {
		return fmt.Errorf("migrate v128→v129 (probe agent_cron_jobs): %w", err)
	}
	if haveCron {
		if err := addColumnIfMissing(tx, "agent_cron_jobs", "queue_when_offline",
			`ALTER TABLE agent_cron_jobs ADD COLUMN queue_when_offline INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v128→v129 (add queue_when_offline): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 129`); err != nil {
		return fmt.Errorf("migrate v128→v129 (stamp version): %w", err)
	}
	return tx.Commit()
}
