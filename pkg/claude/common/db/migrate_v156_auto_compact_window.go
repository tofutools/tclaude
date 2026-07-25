package db

import (
	"database/sql"
	"fmt"
)

// migrateV155toV156 adds the auto-compaction context-window columns:
//
//   - spawn_profiles.auto_compact_window — a saved profile's
//     CLAUDE_CODE_AUTO_COMPACT_WINDOW default, a canonical decimal token count
//     ("450000"), "" = unset.
//   - sessions.auto_compact_window — the window the launch actually resolved to,
//     so a resume / clone / reincarnation keeps compacting at the same point
//     instead of silently reverting the successor to the model's full window.
//
// Stored as TEXT rather than INTEGER for the same reason effort and
// ask_user_question_timeout are: "" is the unset state every launch-field layer
// above already understands, so the value threads through resolveStringLaunchField,
// the profile overlay and the spawn wire without a nullable-integer special case.
//
// Both default to the empty string, which reads as "no window pinned" — exactly
// the posture a legacy row should get, since every existing agent was launched
// against the model's own default compaction threshold.
//
// Additive, probe-guarded ADD COLUMNs in one transaction (the migrateV154toV155
// convention) so a half-applied run converges on re-run.
func migrateV155toV156(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v155→v156 (auto compact window): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	adds := []struct {
		table  string
		column string
		decl   string
	}{
		{"spawn_profiles", "auto_compact_window", "TEXT NOT NULL DEFAULT ''"},
		{"sessions", "auto_compact_window", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, add := range adds {
		var haveTable int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, add.table,
		).Scan(&haveTable); err != nil {
			return fmt.Errorf("migrate v155→v156 (probe %s): %w", add.table, err)
		}
		if haveTable == 0 {
			continue
		}
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, add.table, add.column,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v155→v156 (probe %s.%s): %w", add.table, add.column, err)
		}
		if haveColumn > 0 {
			continue
		}
		if _, err := tx.Exec(
			`ALTER TABLE ` + add.table + ` ADD COLUMN ` + add.column + ` ` + add.decl,
		); err != nil {
			return fmt.Errorf("migrate v155→v156 (add %s.%s): %w", add.table, add.column, err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 156`); err != nil {
		return fmt.Errorf("migrate v155→v156 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v155→v156 (commit): %w", err)
	}
	return nil
}
