package db

import (
	"database/sql"
	"fmt"
)

// migrateV96toV97 adds sessions.ask_user_question_timeout — the resolved Claude
// Code AskUserQuestion idle-timeout (inherit|never|60s|5m|10m, "" = pre-column
// row) a session was spawned under. Recorded once at spawn so a relaunch
// (resume / clone / reincarnate) can PRESERVE it — a reincarnated agentic
// worker comes back on its 5m auto-continue instead of reverting to the
// operator's global settings.json. Modeled on sessions.sandbox_mode (v58), but
// with preserve — not re-default — semantics, since the operator wants the
// per-agent timeout carried across the handoff.
//
// An empty-string DEFAULT (TEXT NOT NULL) back-fills existing rows to unset,
// which the relaunch read treats as "nothing to preserve", falling through to
// the harness default — no data pass. Additive + idempotent (the v76+ convention): the table is
// probed so a partial-schema heal DB missing it is a clean skip; the ADD COLUMN
// is guarded by a pragma_table_info probe so a half-applied run converges on
// re-run instead of wedging on "duplicate column". Rides one transaction with
// the version bump; no VACUUM snapshot — nothing is dropped or rewritten.
func migrateV96toV97(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v96→v97: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "sessions")
	if err != nil {
		return fmt.Errorf("migrate v96→v97 (probe sessions): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'ask_user_question_timeout'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v96→v97 (probe column): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE sessions ADD COLUMN ask_user_question_timeout TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v96→v97 (add column): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 97`); err != nil {
		return fmt.Errorf("migrate v96→v97 (version): %w", err)
	}
	return tx.Commit()
}
