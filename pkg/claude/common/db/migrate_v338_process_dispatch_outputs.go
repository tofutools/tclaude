package db

import (
	"database/sql"
	"fmt"
)

// migrateV337toV338 adds the outputs an agent performer reports to its
// dispatch report (tk_e46af423): the values of the step outputs its node
// declares, as a JSON object of name to value, ” when it declares none. A
// report is stored before it is applied, so the outputs are stored with it,
// and later steps' prompts read them from here.
func migrateV337toV338(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v337→v338 (process dispatch outputs): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('process_agent_dispatches') WHERE name = 'report_outputs'`).Scan(&present); err != nil {
		return fmt.Errorf("migrate v337→v338 (inspect process_agent_dispatches): %w", err)
	}
	if present == 0 {
		if _, err := tx.Exec(`ALTER TABLE process_agent_dispatches ADD COLUMN report_outputs TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(report_outputs AS BLOB)) <= 65536)`); err != nil {
			return fmt.Errorf("migrate v337→v338 (add report_outputs): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 338`); err != nil {
		return fmt.Errorf("migrate v337→v338 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v337→v338 (commit): %w", err)
	}
	return nil
}
