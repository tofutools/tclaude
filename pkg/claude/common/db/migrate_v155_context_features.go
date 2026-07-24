package db

import (
	"database/sql"
	"fmt"
)

// migrateV154toV155 adds the per-agent startup-context trim columns (TCL-597):
//
//   - spawn_profiles.context_features — TEXT, a JSON object of slug → "on"|"off"
//     ("" = the profile trims nothing).
//   - sessions.context_features — TEXT, the map the launch actually resolved to,
//     so a resume / clone / reincarnation reproduces the same lean context
//     instead of silently handing the successor the full harness startup load.
//
// Both default to the empty string, which reads as "no overrides" — exactly the
// posture a legacy row should get, since every existing agent was launched
// against Claude Code's untrimmed startup context.
//
// Additive, probe-guarded ADD COLUMNs in one transaction (the migrateV110toV111
// convention) so a half-applied run converges on re-run.
func migrateV154toV155(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v154→v155 (context features): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	adds := []struct {
		table  string
		column string
		decl   string
	}{
		{"spawn_profiles", "context_features", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "context_features", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, add := range adds {
		var haveTable int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, add.table,
		).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v154→v155 (probe %s): %w", add.table, err)
		}
		if haveTable == 0 {
			continue
		}
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, add.table, add.column,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v154→v155 (probe %s.%s): %w", add.table, add.column, err)
		}
		if haveColumn > 0 {
			continue
		}
		if _, err := tx.Exec(
			`ALTER TABLE ` + add.table + ` ADD COLUMN ` + add.column + ` ` + add.decl,
		); err != nil {
			return fmt.Errorf("migrate v154→v155 (add %s.%s): %w", add.table, add.column, err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 155`); err != nil {
		return fmt.Errorf("migrate v154→v155 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v154→v155 (commit): %w", err)
	}
	return nil
}
