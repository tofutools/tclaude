package db

import (
	"database/sql"
	"fmt"
)

// migrateV160toV161 lets a spawn profile pin which implementation owns launch
// confinement, so `tclaude-layer` becomes selectable per spawn rather than only
// through a direct `session new --sandbox-impl` (TCL-769).
//
// The default is the EMPTY STRING, deliberately unlike the sessions column
// added in v160, which defaults to 'harness-builtin'. The two columns record
// different things and the difference is load-bearing:
//
//   - sessions.sandbox_implementation records what a launch actually ran under.
//     A legacy row ran under the harness's own sandbox, so 'harness-builtin' is
//     the truthful value.
//   - spawn_profiles.sandbox_implementation records what the OPERATOR asked to
//     pin. Every pre-existing profile pinned nothing, and "" is the unset state
//     that falls through to the next tier of the launch-field precedence chain.
//     Defaulting these rows to 'harness-builtin' would silently convert every
//     existing profile into one that PINS the legacy implementation, overriding
//     lower tiers that previously won. That is unobservable today (nothing sets
//     tclaude-layer yet) and would become a real behavior change the moment a
//     group or global default profile opted in.
//
// Additive, probe-guarded ADD COLUMN in one transaction (the v156 convention)
// so a half-applied run converges on re-run.
func migrateV160toV161(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v160→v161 (spawn profile sandbox implementation): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'spawn_profiles'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v160→v161 (spawn profile sandbox implementation): probe table: %w", err)
	}
	if haveTable > 0 {
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'sandbox_implementation'`,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v160→v161 (spawn profile sandbox implementation): probe column: %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE spawn_profiles ADD COLUMN sandbox_implementation TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v160→v161 (spawn profile sandbox implementation): add column: %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 161`); err != nil {
		return fmt.Errorf("migrate v160→v161 (spawn profile sandbox implementation): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v160→v161 (spawn profile sandbox implementation): commit: %w", err)
	}
	return nil
}
