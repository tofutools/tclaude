package db

import (
	"database/sql"
	"fmt"
)

// migrateV176toV177 adds sessions.monitors_json — the per-session ledger
// of Claude Code monitors (the `Monitor` tool) believed to be running,
// keyed by the harness's taskId. See MonitorSet in monitors.go for why it
// is a ledger with a liveness reconcile rather than a counter, and why it
// is separate from bg_shells_json despite sharing a task id namespace.
//
// The column mirrors bg_shells_json: TEXT NOT NULL DEFAULT '', where ''
// means "empty ledger" — the correct reading for every legacy row, since a
// monitor cannot outlive the harness process that wrote the row.
//
// Additive, probe-guarded ADD COLUMN in one transaction (the
// migrateV140toV141 convention) so a half-applied run converges on re-run.
func migrateV176toV177(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v176→v177 (monitors): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v176→v177 (probe sessions): %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'monitors_json'`,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v176→v177 (probe sessions.monitors_json): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE sessions ADD COLUMN monitors_json TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v176→v177 (add sessions.monitors_json): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 177`); err != nil {
		return fmt.Errorf("migrate v176→v177 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v176→v177 (commit): %w", err)
	}
	return nil
}
