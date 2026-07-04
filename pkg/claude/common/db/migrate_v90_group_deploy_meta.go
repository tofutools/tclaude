package db

import (
	"database/sql"
	"fmt"
)

// migrateV89toV90 adds the deployment-provenance columns to agent_groups
// (JOH-245): mission (the free-text or Linear-link topic a task force was
// deployed against) and source_template (the template a group was
// instantiated / deployed from). They let the dashboard show groups as
// deployed "task forces" — units framed by what they were dispatched from and
// against — instead of anonymous groups.
//
// Both are set by the deploy verb; source_template is also set by a plain
// template instantiate (deploy is instantiate with a mission). A group created
// any other way carries "" for both, which the dashboard reads as "not a
// deployed force".
//
// Additive + idempotent (the v76–v89 convention): the table is probed so a
// partial-schema heal DB is a clean skip; each ADD COLUMN is guarded by a
// pragma_table_info probe so a half-applied run converges on re-run instead of
// wedging on "duplicate column". TEXT NOT NULL DEFAULT '' back-fills every
// existing group with no deployment metadata in one data-pass-free ALTER. One
// transaction with the version bump; nothing is dropped or rewritten.
func migrateV89toV90(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v89→v90: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agent_groups")
	if err != nil {
		return fmt.Errorf("migrate v89→v90 (probe agent_groups): %w", err)
	}
	if haveTable {
		// Each new column is TEXT NOT NULL DEFAULT '' — "" = no deployment
		// provenance. Probe-guard each so re-running after a partial apply is a
		// clean no-op instead of a duplicate-column error.
		for _, col := range []string{"mission", "source_template"} {
			var have int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('agent_groups') WHERE name = ?`, col,
			).Scan(&have); err != nil {
				return fmt.Errorf("migrate v89→v90 (probe agent_groups.%s): %w", col, err)
			}
			if have == 0 {
				if _, err := tx.Exec(fmt.Sprintf(
					`ALTER TABLE agent_groups ADD COLUMN %s TEXT NOT NULL DEFAULT ''`, col,
				)); err != nil {
					return fmt.Errorf("migrate v89→v90 (add agent_groups.%s): %w", col, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 90`); err != nil {
		return fmt.Errorf("migrate v89→v90 (version): %w", err)
	}
	return tx.Commit()
}
