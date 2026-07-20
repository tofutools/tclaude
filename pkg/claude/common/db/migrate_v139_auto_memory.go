package db

import (
	"database/sql"
	"fmt"
)

// migrateV138toV139 adds the auto-memory posture columns:
//
//   - spawn_profiles.auto_memory — nullable INTEGER, the profile's tri-state
//     opt-in (NULL = unset, 0 = auto memory off, 1 = auto memory on).
//   - sessions.auto_memory — NOT NULL INTEGER, the resolved posture the launch
//     actually ran with, so a resume reproduces it instead of silently falling
//     back to the harness default.
//
// Claude Code's auto-memory system writes per-project memory files that several
// tclaude agents working the same repo would cross-pollute, so tclaude resolves
// an unset profile to OFF and injects CLAUDE_CODE_DISABLE_AUTO_MEMORY at spawn.
// The sessions column's 0 default therefore reads as "auto memory off" for
// legacy rows, which is exactly the posture a resumed legacy session should get.
//
// Additive, probe-guarded ADD COLUMNs in one transaction (the migrateV110toV111
// convention) so a half-applied run converges on re-run.
func migrateV138toV139(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v138→v139 (auto memory): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	adds := []struct {
		table  string
		column string
		decl   string
	}{
		{"spawn_profiles", "auto_memory", "INTEGER"},
		{"sessions", "auto_memory", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, add := range adds {
		var haveTable int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, add.table,
		).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v138→v139 (probe %s): %w", add.table, err)
		}
		if haveTable == 0 {
			continue
		}
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, add.table, add.column,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v138→v139 (probe %s.%s): %w", add.table, add.column, err)
		}
		if haveColumn > 0 {
			continue
		}
		if _, err := tx.Exec(
			`ALTER TABLE ` + add.table + ` ADD COLUMN ` + add.column + ` ` + add.decl,
		); err != nil {
			return fmt.Errorf("migrate v138→v139 (add %s.%s): %w", add.table, add.column, err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 139`); err != nil {
		return fmt.Errorf("migrate v138→v139 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v138→v139 (commit): %w", err)
	}
	return nil
}
