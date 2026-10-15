package db

import (
	"database/sql"
	"fmt"
)

// migrateV265toV266 adds an agent decider's verdict to its dispatch report
// (tcl-eu70x9). A report is stored before it is applied to its run, so the
// verdict is stored with the outcome and note: the recovery sweep applies a
// stored report exactly as it was sent. Empty for every task's report.
func migrateV265toV266(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v265→v266 (process dispatch verdict): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('process_agent_dispatches') WHERE name = 'report_verdict'`).Scan(&present); err != nil {
		return fmt.Errorf("migrate v265→v266 (inspect process_agent_dispatches): %w", err)
	}
	if present == 0 {
		if _, err := tx.Exec(`ALTER TABLE process_agent_dispatches ADD COLUMN report_verdict TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(report_verdict AS BLOB)) <= 256)`); err != nil {
			return fmt.Errorf("migrate v265→v266 (add report_verdict): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 266`); err != nil {
		return fmt.Errorf("migrate v265→v266 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v265→v266 (commit): %w", err)
	}
	return nil
}
