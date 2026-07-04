package db

import (
	"database/sql"
	"fmt"
)

// migrateV90toV91 stands up the role library (JOH-240): a `roles` table of
// named, reusable defaults (a canonical role-brief + a default launch shape +
// a default permission set) and a `role_ref` column on group_template_agents
// so a template roster agent can REFERENCE a role and inherit its defaults.
//
// The roles table mirrors spawn_profiles' launch fields (harness / model /
// effort / sandbox / approval — TEXT, "" = inherit) plus the permissions JSON
// list group_template_agents carries; brief is the canonical role-brief text.
// role_ref is the by-name reference on the template agent (no DB-level FK —
// existence is validated at the wire boundary, following spawn_profile); ""
// means "no role", so a pre-existing template agent reads as unreferenced.
//
// The canonical seed rows are NOT written here — seeding is self-healing
// (ensureSeededRoles, run on every Open), so a seed a user later deletes
// reappears while their edits stay sacred, per the repo's "self-healing over
// one-shot migrations" principle.
//
// Additive + idempotent (the v76–v90 convention): CREATE TABLE IF NOT EXISTS
// is a clean re-run, and the role_ref ADD COLUMN is guarded by a
// sqlite_master table probe AND a pragma_table_info column probe so a
// half-applied run converges on re-run instead of wedging on "duplicate
// column". One transaction with the version bump; nothing is dropped or
// rewritten.
func migrateV90toV91(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v90→v91: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS roles (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT NOT NULL UNIQUE,
			descr         TEXT NOT NULL DEFAULT '',
			brief         TEXT NOT NULL DEFAULT '',
			spawn_profile TEXT NOT NULL DEFAULT '',
			harness       TEXT NOT NULL DEFAULT '',
			model         TEXT NOT NULL DEFAULT '',
			effort        TEXT NOT NULL DEFAULT '',
			sandbox       TEXT NOT NULL DEFAULT '',
			approval      TEXT NOT NULL DEFAULT '',
			permissions   TEXT NOT NULL DEFAULT '[]',
			created_at    TEXT NOT NULL,
			updated_at    TEXT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("migrate v90→v91 (create roles): %w", err)
	}

	// role_ref on group_template_agents — the by-name role reference. Guarded
	// on the table existing (a minimally-seeded migration-heal DB may not have
	// created it) AND the column being absent (converge on re-run).
	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'group_template_agents'`,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v90→v91 (probe group_template_agents): %w", err)
	}
	if haveTable > 0 {
		var haveCol int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('group_template_agents') WHERE name = 'role_ref'`,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v90→v91 (probe group_template_agents.role_ref): %w", err)
		}
		if haveCol == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE group_template_agents ADD COLUMN role_ref TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v90→v91 (add group_template_agents.role_ref): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 91`); err != nil {
		return fmt.Errorf("migrate v90→v91 (version): %w", err)
	}
	return tx.Commit()
}
