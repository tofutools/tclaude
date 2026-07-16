package db

import (
	"database/sql"
	"fmt"
)

// migrateV125toV126 adds the persisted opt-in for an immediate cron fire.
// Existing jobs default to false: migration and daemon restart must never
// reinterpret a historical job as requesting a fresh immediate delivery.
func migrateV125toV126(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v125→v126 (cron run immediately): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveCron, err := txTableExists(tx, "agent_cron_jobs")
	if err != nil {
		return fmt.Errorf("migrate v125→v126 (probe agent_cron_jobs): %w", err)
	}
	if haveCron {
		if err := addColumnIfMissing(tx, "agent_cron_jobs", "run_immediately",
			`ALTER TABLE agent_cron_jobs ADD COLUMN run_immediately INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v125→v126 (add run_immediately): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 126`); err != nil {
		return fmt.Errorf("migrate v125→v126 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v125→v126 (cron run immediately): commit: %w", err)
	}
	return nil
}
