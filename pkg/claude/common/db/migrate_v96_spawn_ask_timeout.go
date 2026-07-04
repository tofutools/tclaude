package db

import (
	"database/sql"
	"fmt"
)

// migrateV95toV96 adds spawn_profiles.ask_user_question_timeout — a saved
// profile's Claude Code AskUserQuestion idle-timeout default (never|60s|5m|10m,
// "" = unset), which the spawn dialog pre-fills and the daemon delivers
// per-spawn as a `--settings` override. TEXT NOT NULL DEFAULT '' so existing
// profiles back-fill to "unset" (the agent keeps using the operator's own
// settings.json) with no data pass.
//
// Additive + idempotent (the v76+ convention): the table is probed so a
// partial-schema heal DB missing it is a clean skip; the ADD COLUMN is guarded
// by a pragma_table_info probe so a half-applied run converges on re-run instead
// of wedging on "duplicate column". Rides one transaction with the version bump;
// no VACUUM snapshot — nothing is dropped or rewritten.
func migrateV95toV96(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v95→v96: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "spawn_profiles")
	if err != nil {
		return fmt.Errorf("migrate v95→v96 (probe spawn_profiles): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'ask_user_question_timeout'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v95→v96 (probe column): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE spawn_profiles ADD COLUMN ask_user_question_timeout TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v95→v96 (add column): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 96`); err != nil {
		return fmt.Errorf("migrate v95→v96 (version): %w", err)
	}
	return tx.Commit()
}
