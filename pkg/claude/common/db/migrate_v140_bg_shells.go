package db

import (
	"database/sql"
	"fmt"
)

// migrateV139toV140 adds sessions.bg_shells_json — the per-session ledger
// of Claude Code background shell commands (`Bash` with
// `run_in_background: true`) believed to be running, keyed by
// backgroundTaskId. See BgShellSet in bgshells.go for why it is a ledger
// with a liveness reconcile rather than a counter.
//
// The column mirrors subagents_json: TEXT NOT NULL DEFAULT '', where ''
// means "empty ledger" — which is the correct reading for every legacy
// row, since a background shell cannot outlive the harness process that
// wrote the row.
//
// Additive, probe-guarded ADD COLUMN in one transaction (the
// migrateV110toV111 convention) so a half-applied run converges on re-run.
func migrateV139toV140(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v139→v140 (bg shells): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v139→v140 (probe sessions): %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'bg_shells_json'`,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v139→v140 (probe sessions.bg_shells_json): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE sessions ADD COLUMN bg_shells_json TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v139→v140 (add sessions.bg_shells_json): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 140`); err != nil {
		return fmt.Errorf("migrate v139→v140 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v139→v140 (commit): %w", err)
	}
	return nil
}
