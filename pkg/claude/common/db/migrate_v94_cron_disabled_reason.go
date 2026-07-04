package db

import (
	"database/sql"
	"fmt"
)

// migrateV93toV94 adds agent_cron_jobs.disabled_reason — a marker that
// distinguishes a cron job tclaude auto-disabled from one the human paused by
// hand (JOH-345).
//
// When retiring a deployed task force empties its group (no live members left),
// the group's template-seeded rhythm jobs would otherwise keep firing every
// interval forever, delivering to nobody. The wind-down instead DISABLES those
// group-target jobs (non-destructive, visible + reversible in the Cron tab)
// and stamps disabled_reason = 'group-retired'. A later `groups resume` on that
// group re-enables exactly the jobs carrying that marker — and deliberately NOT
// a job the human disabled by hand (which keeps disabled_reason = '').
//
// One additive column, following the v76–v93 convention (probe-guarded ADD
// COLUMN, one transaction with the version bump — a half-applied run converges
// on re-run):
//
//   - agent_cron_jobs.disabled_reason — '' (the default: a normal, human-managed
//     enable/disable state) or 'group-retired' (auto-disabled by a retire that
//     emptied the group). Rides the same TEXT-default convention as the v93
//     target_role column.
func migrateV93toV94(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v93→v94: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// agent_cron_jobs.disabled_reason — probe-guarded on the table existing AND
	// the column being absent (converge on re-run).
	haveCron, err := txTableExists(tx, "agent_cron_jobs")
	if err != nil {
		return fmt.Errorf("migrate v93→v94 (probe agent_cron_jobs): %w", err)
	}
	if haveCron {
		if err := addColumnIfMissing(tx, "agent_cron_jobs", "disabled_reason",
			`ALTER TABLE agent_cron_jobs ADD COLUMN disabled_reason TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate v93→v94: %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 94`); err != nil {
		return fmt.Errorf("migrate v93→v94 (version): %w", err)
	}
	return tx.Commit()
}
