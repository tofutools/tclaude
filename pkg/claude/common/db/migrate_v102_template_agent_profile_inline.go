package db

import (
	"database/sql"
	"fmt"
)

// migrateV101toV102 adds group_template_agents.profile_inline — a template-
// LOCAL spawn profile for one roster agent (JOH-350 follow-up). It carries the
// same shape as a spawn_profiles row (launch fields, birth-time owner /
// permission overrides, launch toggles), serialized as a JSON object, but
// lives inside the template instead of the shared profile registry: a
// blueprint can give one agent a bespoke launch config without polluting the
// library, and the template stays portable (export/import carries the config
// with no external profile reference to recreate).
//
// "" = none (every existing row). Resolution order at instantiate: per-agent
// legacy inline fields → profile_inline → referenced spawn_profile → role
// tiers → harness default.
//
// Additive + idempotent (the v76+ convention): the table is probed so a
// partial-schema heal DB is a clean skip; the ADD COLUMN is guarded by a
// pragma_table_info probe so a half-applied run converges on re-run instead of
// wedging on "duplicate column". One transaction with the version bump;
// nothing is dropped or rewritten.
func migrateV101toV102(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v101→v102: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "group_template_agents")
	if err != nil {
		return fmt.Errorf("migrate v101→v102 (probe group_template_agents): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('group_template_agents') WHERE name = 'profile_inline'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v101→v102 (probe column): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE group_template_agents ADD COLUMN profile_inline TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v101→v102 (add column): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 102`); err != nil {
		return fmt.Errorf("migrate v101→v102 (version): %w", err)
	}
	return tx.Commit()
}
