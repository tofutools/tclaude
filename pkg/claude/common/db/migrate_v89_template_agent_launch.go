package db

import (
	"database/sql"
	"fmt"
)

// migrateV88toV89 adds the per-agent launch profile columns to
// group_template_agents (JOH-239): spawn_profile (a by-name reference to a
// spawn_profiles row, no DB-level FK — validated at the boundary) plus the
// inline launch overrides harness / model / effort / sandbox / approval. They
// let a template give each role a distinct launch shape — a lead on Opus-high,
// the tester on something cheap — instead of every spawned agent inheriting the
// group's single default spawn profile.
//
// Resolution order at instantiate: per-agent inline override → referenced
// profile → group default profile → harness default. These columns carry the
// first two tiers; the last two already resolve inside executeSpawn.
//
// Additive + idempotent (the v76–v88 convention): the table is probed so a
// partial-schema heal DB is a clean skip; each ADD COLUMN is guarded by a
// pragma_table_info probe so a half-applied run converges on re-run instead of
// wedging on "duplicate column". TEXT NOT NULL DEFAULT '' back-fills every
// existing template agent with no launch override (today's behaviour: inherit
// the group default) in one data-pass-free ALTER. One transaction with the
// version bump; nothing is dropped or rewritten.
func migrateV88toV89(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v88→v89: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "group_template_agents")
	if err != nil {
		return fmt.Errorf("migrate v88→v89 (probe group_template_agents): %w", err)
	}
	if haveTable {
		// Each new column is TEXT NOT NULL DEFAULT '' — "" = no inline override /
		// no profile reference, which the resolver reads as "fall through to the
		// next tier". Probe-guard each so re-running after a partial apply is a
		// clean no-op instead of a duplicate-column error.
		for _, col := range []string{"spawn_profile", "harness", "model", "effort", "sandbox", "approval"} {
			var have int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('group_template_agents') WHERE name = ?`, col,
			).Scan(&have); err != nil {
				return fmt.Errorf("migrate v88→v89 (probe group_template_agents.%s): %w", col, err)
			}
			if have == 0 {
				if _, err := tx.Exec(fmt.Sprintf(
					`ALTER TABLE group_template_agents ADD COLUMN %s TEXT NOT NULL DEFAULT ''`, col,
				)); err != nil {
					return fmt.Errorf("migrate v88→v89 (add group_template_agents.%s): %w", col, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 89`); err != nil {
		return fmt.Errorf("migrate v88→v89 (version): %w", err)
	}
	return tx.Commit()
}
