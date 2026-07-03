package db

import (
	"database/sql"
	"fmt"
)

// migrateV86toV87 adds `group_templates.work_pattern` — the template's
// default work pattern (JOH-336): a JSON array of ordered, routed
// briefing messages ({send_to, value}) delivered after a template's
// whole roster has spawned. send_to names a roster agent or "all";
// value may carry a {{task}} placeholder replaced with the
// per-instantiation task at delivery. '' (the back-fill) and "[]" both
// read back as "no pattern", so existing templates keep today's
// spawn-only behaviour.
//
// Additive + idempotent (the v76–v86 convention): the table is probed so
// a partial-schema heal DB without `group_templates` is a clean skip;
// the ADD COLUMN is guarded by a pragma_table_info probe so a
// half-applied run converges on re-run instead of wedging on "duplicate
// column". TEXT NOT NULL DEFAULT '' back-fills every existing row with
// no data pass. One transaction with the version bump; nothing is
// dropped or rewritten.
func migrateV86toV87(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v86→v87: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "group_templates")
	if err != nil {
		return fmt.Errorf("migrate v86→v87 (probe group_templates): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('group_templates') WHERE name = 'work_pattern'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v86→v87 (probe group_templates.work_pattern): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE group_templates ADD COLUMN work_pattern TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v86→v87 (add group_templates.work_pattern): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 87`); err != nil {
		return fmt.Errorf("migrate v86→v87 (version): %w", err)
	}
	return tx.Commit()
}
