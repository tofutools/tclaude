package db

import (
	"database/sql"
	"fmt"
)

// migrateV153toV154 adds the cron job's explicit human-operator attribution.
// A group-target job a human schedules from the dashboard without picking an
// owner agent fans out as the operator/human rather than a sender-less system
// message; this flag records that intent so the fire path can tag every
// delivered row operator-authored (empty owner alone is NOT a safe signal —
// template rhythms and other paths also leave the owner empty). Existing jobs
// default to 0: unchanged agent/owner attribution.
func migrateV153toV154(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	haveCron, err := txTableExists(tx, "agent_cron_jobs")
	if err != nil {
		return fmt.Errorf("migrate v153→v154 (probe agent_cron_jobs): %w", err)
	}
	if haveCron {
		if err := addColumnIfMissing(tx, "agent_cron_jobs", "operator_authored",
			`ALTER TABLE agent_cron_jobs ADD COLUMN operator_authored INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v153→v154 (add operator_authored): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 154`); err != nil {
		return fmt.Errorf("migrate v153→v154 (stamp version): %w", err)
	}
	return tx.Commit()
}
