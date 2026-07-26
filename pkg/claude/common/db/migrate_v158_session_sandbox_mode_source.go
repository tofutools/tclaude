package db

import (
	"database/sql"
	"fmt"
)

// migrateV157toV158 adds sessions.sandbox_mode_source — which resolution tier
// CHOSE the launch's sandbox mode.
//
// sessions.sandbox_mode records the mode itself, and os_sandbox_state/_source
// (v157) record whether the OS sandbox was actually active and what decided
// that. Neither can say who asked for the mode. The daemon spawn boundary
// resolves it through a tier stack — an explicit flag, then a named spawn
// profile, then the group's default, then the global default — so an operator
// who never typed `--sandbox on` still gets one when a default profile carries
// it, and the badge attributed that to "this launch" as though they had chosen
// it themselves. The tier is computed today (resolveStringLaunchField) and then
// discarded; this column is where it comes to rest.
//
// It is a LAUNCH record, like the columns beside it: resolved once, at the
// boundary, against the state as it was. The durable relaunch profile projects
// from it so a resumed agent replays the attribution instead of degrading to an
// anonymous "this launch" after its first restart.
//
// Defaults to the empty string — "nothing recorded" — which is what every
// pre-column row genuinely is, and what a direct `tclaude session new` with no
// attribution keeps writing. Empty renders exactly as it did before the column
// existed, so no legacy row is retroactively credited to a profile.
//
// Additive, probe-guarded ADD COLUMN in one transaction (the migrateV110toV111
// convention) so a half-applied run converges on re-run.
func migrateV157toV158(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v157→v158 (session sandbox mode source): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v157→v158 (probe sessions): %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'sandbox_mode_source'`,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v157→v158 (probe sessions.sandbox_mode_source): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE sessions ADD COLUMN sandbox_mode_source TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v157→v158 (add sessions.sandbox_mode_source): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 158`); err != nil {
		return fmt.Errorf("migrate v157→v158 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v157→v158 (commit): %w", err)
	}
	return nil
}
