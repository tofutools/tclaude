package db

import (
	"database/sql"
	"fmt"
)

// migrateV94toV95 adds the per-agent task-reference link:
// `agents.task_ref_url` (an http(s) URL pointing at the work item this
// agent is on — a Linear issue, a GitHub issue/PR, a ticket, …) and
// `agents.task_ref_label` (an optional human-set display label; when
// empty the dashboard/CLI derive one from the URL — Linear → JOH-xxx,
// GitHub → #nnn, else the host). Stored per-agent (keyed by agent_id)
// rather than per-group-membership so it survives conv rotation
// (reincarnate/clone), an agent can set its OWN link with no group
// argument, and one link renders across every group the agent is in.
//
// Additive + idempotent (the v76–v84 convention): the table is probed so
// a partial-schema heal DB without `agents` is a clean skip; each ADD
// COLUMN is guarded by a pragma_table_info probe so a half-applied run
// converges on re-run instead of wedging on "duplicate column".
// NOT NULL DEFAULT ” back-fills every existing row to "no task link"
// with no data pass. One transaction with the version bump; nothing is
// dropped or rewritten.
func migrateV94toV95(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v94→v95: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agents")
	if err != nil {
		return fmt.Errorf("migrate v94→v95 (probe agents): %w", err)
	}
	if haveTable {
		for _, col := range []string{"task_ref_url", "task_ref_label"} {
			var have int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = ?`, col,
			).Scan(&have); err != nil {
				return fmt.Errorf("migrate v94→v95 (probe agents.%s): %w", col, err)
			}
			if have == 0 {
				if _, err := tx.Exec(
					`ALTER TABLE agents ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`,
				); err != nil {
					return fmt.Errorf("migrate v94→v95 (add agents.%s): %w", col, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 95`); err != nil {
		return fmt.Errorf("migrate v94→v95 (version): %w", err)
	}
	return tx.Commit()
}
