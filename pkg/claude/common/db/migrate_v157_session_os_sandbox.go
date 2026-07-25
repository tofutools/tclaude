package db

import (
	"database/sql"
	"fmt"
)

// migrateV156toV157 adds the launch-time OS-sandbox verdict columns (TCL-729):
//
//   - sessions.os_sandbox_state — "on" | "off" | "unconfigured", the answer to
//     "was the harness's OS sandbox actually active for this launch".
//   - sessions.os_sandbox_source — what decided it (the launch flag, or the
//     settings file that won the precedence chain).
//   - sessions.os_sandbox_unverified — 1 when a file OUTRANKING the deciding
//     tier could not be read or parsed, so a policy tclaude never saw may say
//     the opposite. The badge is a durable claim about containment, so the doubt
//     has to be stored with the verdict rather than left on the spawn's stderr.
//
// They exist because `sessions.sandbox_mode` records the launch REQUEST, not
// the outcome. For Claude Code the default request is `inherit` — deliberately
// no `--settings` override, so the operator's own settings.json posture
// survives — which means the mode alone cannot say whether anything confines
// the agent, and the dashboard badge driven off it stayed blank for exactly the
// posture tclaude recommends.
//
// Both default to the empty string: "nothing recorded", which is what every
// pre-column row genuinely is (and what a harness whose mode already states the
// posture keeps writing). Empty renders as it always did — no badge — so a
// legacy row is not retroactively claimed to be sandboxed or unsandboxed.
//
// Additive, probe-guarded ADD COLUMNs in one transaction (the migrateV110toV111
// convention) so a half-applied run converges on re-run.
func migrateV156toV157(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v156→v157 (session os sandbox): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v156→v157 (probe sessions): %w", err)
	}
	if haveTable > 0 {
		for _, add := range []struct{ column, decl string }{
			{"os_sandbox_state", "TEXT NOT NULL DEFAULT ''"},
			{"os_sandbox_source", "TEXT NOT NULL DEFAULT ''"},
			{"os_sandbox_unverified", "INTEGER NOT NULL DEFAULT 0"},
		} {
			var haveColumn int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, add.column,
			).Scan(&haveColumn); err != nil {
				return fmt.Errorf("migrate v156→v157 (probe sessions.%s): %w", add.column, err)
			}
			if haveColumn > 0 {
				continue
			}
			if _, err := tx.Exec(
				`ALTER TABLE sessions ADD COLUMN ` + add.column + ` ` + add.decl,
			); err != nil {
				return fmt.Errorf("migrate v156→v157 (add sessions.%s): %w", add.column, err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 157`); err != nil {
		return fmt.Errorf("migrate v156→v157 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v156→v157 (commit): %w", err)
	}
	return nil
}
